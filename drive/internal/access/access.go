// Package access is the single place where "may this principal touch this
// node?" is decided. Every handler that reads or changes a node must go
// through Resolve; there are no other authorization checks in the code base.
package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Level is what a principal may do with a node. Levels are ordered.
type Level int

const (
	None   Level = iota // no access; handlers answer 404
	Viewer              // list, download, preview
	Editor              // + create, upload, rename, move, trash inside the shared folder
	Owner               // + share, restore, delete forever
)

func (l Level) String() string {
	switch l {
	case Viewer:
		return "viewer"
	case Editor:
		return "editor"
	case Owner:
		return "owner"
	}
	return "none"
}

// MaxDepth bounds the folder depth. Resolve fails closed beyond it.
const MaxDepth = 128

// ErrNotFound is returned when the node does not exist.
var ErrNotFound = errors.New("node not found")

// Node is one row of the nodes table.
type Node struct {
	ID        string
	OwnerID   int64
	ParentID  string // "" for a root
	Name      string
	IsDir     bool
	BlobSHA   string // "" for folders
	Size      int64
	CreatedAt int64
	UpdatedAt int64
	TrashedAt int64 // 0 when not directly trashed
	TrashedBy int64 // user who trashed it, 0 when not trashed
}

// NodeColumns is the column list matching ScanNode, optionally prefixed.
const NodeColumns = `id, owner_id, COALESCE(parent_id, ''), name, is_dir, COALESCE(blob_sha, ''), size, created_at, updated_at, COALESCE(trashed_at, 0), COALESCE(trashed_by, 0)`

// ScanNode scans a row selected with NodeColumns.
func ScanNode(sc interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	err := sc.Scan(&n.ID, &n.OwnerID, &n.ParentID, &n.Name, &n.IsDir, &n.BlobSHA, &n.Size, &n.CreatedAt, &n.UpdatedAt, &n.TrashedAt, &n.TrashedBy)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// Principal is who is asking. Exactly one of the two forms is used:
//   - a signed-in user (UserID and Email set), or
//   - a visitor holding a valid public link (LinkNodeID set). The caller must
//     have validated the link (exists, not expired, password given) first.
type Principal struct {
	UserID     int64
	Email      string // lower-cased
	LinkNodeID string
}

// Result is the outcome of Resolve.
type Result struct {
	Level Level
	Node  *Node
	// Chain is the node followed by its ancestors up to the root.
	Chain []*Node
	// GrantIdx is the index in Chain of the topmost node the principal may
	// see (the root for owners, the outermost shared folder for share
	// recipients, the linked node for link visitors). Ancestors above it must
	// never be shown to the principal.
	GrantIdx int
	// Trashed is true when the node or one of its ancestors is in the trash.
	// Only owners get a non-None level for trashed nodes.
	Trashed bool
}

// Visible returns the part of the chain the principal may see, root-most
// first: the breadcrumb trail.
func (r Result) Visible() []*Node {
	if r.Level == None {
		return nil
	}
	out := make([]*Node, 0, r.GrantIdx+1)
	for i := r.GrantIdx; i >= 0; i-- {
		out = append(out, r.Chain[i])
	}
	return out
}

// Querier is satisfied by *sql.DB and *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Resolve computes the principal's access level for a node.
//
// Rules:
//   - The owner of the node has Owner.
//   - Otherwise the level is the strongest share addressed to the principal's
//     email on the node or any ancestor (permissions inherit downwards).
//   - A link visitor has Viewer on the linked node and its descendants only.
//   - A node in the trash (itself or through an ancestor) is visible to its
//     owner only.
//   - Anything else, including broken or too-deep chains, is None.
func Resolve(ctx context.Context, q Querier, p Principal, nodeID string) (Result, error) {
	if nodeID == "" {
		return Result{}, ErrNotFound
	}
	chain, err := loadChain(ctx, q, nodeID)
	if err != nil {
		return Result{}, err
	}
	res := Result{Node: chain[0], Chain: chain}
	for _, n := range chain {
		if n.TrashedAt != 0 {
			res.Trashed = true
		}
	}

	if p.LinkNodeID != "" {
		// Link visitors: never combined with a user identity.
		if res.Trashed {
			return res, nil
		}
		for i, n := range chain {
			if n.ID == p.LinkNodeID {
				res.Level = Viewer
				res.GrantIdx = i
				break
			}
		}
		return res, nil
	}

	if p.UserID == 0 {
		return res, nil
	}
	if chain[0].OwnerID == p.UserID {
		res.Level = Owner
		res.GrantIdx = len(chain) - 1
		return res, nil
	}
	if res.Trashed || p.Email == "" {
		return res, nil
	}

	// Shares on the node or any ancestor.
	args := make([]any, 0, len(chain)+1)
	args = append(args, p.Email)
	for _, n := range chain {
		args = append(args, n.ID)
	}
	rows, err := q.QueryContext(ctx,
		`SELECT node_id, role FROM shares WHERE email = ? AND node_id IN (?`+strings.Repeat(",?", len(chain)-1)+`)`, args...)
	if err != nil {
		return Result{}, fmt.Errorf("resolve shares: %w", err)
	}
	defer rows.Close()
	roles := map[string]Level{}
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			return Result{}, fmt.Errorf("resolve shares: %w", err)
		}
		switch role {
		case "editor":
			roles[id] = Editor
		case "viewer":
			roles[id] = Viewer
		}
	}
	if err := rows.Err(); err != nil {
		return Result{}, fmt.Errorf("resolve shares: %w", err)
	}
	for i, n := range chain {
		if l, ok := roles[n.ID]; ok {
			if l > res.Level {
				res.Level = l
			}
			res.GrantIdx = i
		}
	}
	return res, nil
}

// loadChain returns the node and all its ancestors, node first. It fails if
// the node does not exist or the chain does not end at a root within MaxDepth.
func loadChain(ctx context.Context, q Querier, id string) ([]*Node, error) {
	rows, err := q.QueryContext(ctx, `
		WITH RECURSIVE chain(id, depth) AS (
			SELECT ?, 0
			UNION ALL
			SELECT n.parent_id, c.depth + 1 FROM nodes n JOIN chain c ON n.id = c.id
			WHERE n.parent_id IS NOT NULL AND c.depth < ?
		)
		SELECT `+prefixed("n")+` FROM chain c JOIN nodes n ON n.id = c.id ORDER BY c.depth`, id, MaxDepth)
	if err != nil {
		return nil, fmt.Errorf("load node chain: %w", err)
	}
	defer rows.Close()
	var chain []*Node
	for rows.Next() {
		n, err := ScanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("load node chain: %w", err)
		}
		chain = append(chain, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load node chain: %w", err)
	}
	if len(chain) == 0 {
		return nil, ErrNotFound
	}
	if chain[len(chain)-1].ParentID != "" {
		return nil, fmt.Errorf("node %s: chain does not reach a root within %d levels", id, MaxDepth)
	}
	return chain, nil
}

func prefixed(alias string) string {
	return alias + `.id, ` + alias + `.owner_id, COALESCE(` + alias + `.parent_id, ''), ` + alias + `.name, ` + alias + `.is_dir, COALESCE(` +
		alias + `.blob_sha, ''), ` + alias + `.size, ` + alias + `.created_at, ` + alias + `.updated_at, COALESCE(` + alias + `.trashed_at, 0), COALESCE(` + alias + `.trashed_by, 0)`
}

// Columns returns NodeColumns with a table alias prefix.
func Columns(alias string) string { return prefixed(alias) }
