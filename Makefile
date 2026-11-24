.PHONY: build test lint clean

build:
	$(MAKE) -C drive build
	cd curator && uv sync

test:
	$(MAKE) -C drive test
	cd curator && uv run pytest -q
	deploy/tests/run.sh

lint:
	$(MAKE) -C drive lint
	cd curator && uv run ruff check . && uv run ruff format --check .

clean:
	rm -rf drive/bin curator/.venv
