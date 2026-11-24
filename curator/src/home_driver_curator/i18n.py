"""Every user-visible string, Vietnamese and English, keyed by id (docs/DESIGN.md "Language").

Templates call ``t("key", n=3)``. English keys may have a ``_one`` variant used when n == 1.
Strings listed in JS_KEYS are also handed to static/app.js through a data attribute.
"""

from __future__ import annotations

from collections.abc import Callable

LANGS = ("vi", "en")
DEFAULT_LANG = "en"

STRINGS: dict[str, dict[str, str]] = {
    "en": {
        "app_name": "Photo clean-up",
        "nav_home": "Overview",
        "nav_activity": "History",
        "nav_drive": "Drive",
        "menu": "Settings",
        "language": "Language",
        "theme": "Appearance",
        "theme_auto": "Automatic",
        "theme_light": "Light",
        "theme_dark": "Dark",
        "signed_in_as": "Signed in as {user}",
        "back": "Back",
        "cat_blurry": "Blurry photos",
        "cat_dark": "Dark or blank photos",
        "cat_screenshot": "Screenshots",
        "cat_burst": "Near-identical shots",
        "cat_duplicate": "Duplicates",
        "help_blurry": "Photos that came out badly out of focus.",
        "help_dark": "Almost black, almost white or empty frames, like pocket shots.",
        "help_screenshot": "Screen captures, not taken with a camera.",
        "help_burst": "Several shots taken within seconds. Keep the best one.",
        "help_duplicate": "The same photo saved more than once (found by Immich).",
        "reason_blurry": "This photo is blurry",
        "reason_dark": "This photo is dark or blank",
        "reason_screenshot": "This is a screenshot",
        "reason_burst": "{n} near-identical photos",
        "reason_duplicate": "{n} copies of the same photo",
        "home_title": "Tidy up your photos",
        "home_lead": "These photos may not be worth keeping. You decide; nothing is removed "
        "without you.",
        "home_count": "{n} photos to look at",
        "home_count_one": "1 photo to look at",
        "home_start": "Start reviewing",
        "home_all_done": "All tidy. There is nothing to review right now.",
        "home_all_done_hint": "New photos are checked every night.",
        "count_photos": "{n} photos",
        "count_photos_one": "1 photo",
        "none_left": "Nothing here",
        "turned_off": "Turned off",
        "safe_note": "Photos you remove go to the Immich trash and can be brought back for "
        "30 days.",
        "video_note": "Only photos are checked. Videos are never suggested.",
        "dry_run_note": "Practice mode is on: nothing is changed in Immich.",
        "scan_title": "Nightly check",
        "scan_never": "The library has not been checked yet.",
        "scan_running": "A check is running now.",
        "scan_completed": "Last checked {when}.",
        "scan_interrupted": "The last check was stopped early. It continues next time.",
        "scan_failed": "The last check could not finish. Immich may have been unreachable.",
        "scan_progress": "{done} of {total} photos checked so far.",
        "scan_remaining": "{n} photos are still waiting to be checked on the next nights.",
        "kept_forever": "{n} photos marked to keep forever.",
        "details": "Details",
        "left": "{n} left",
        "left_one": "1 left",
        "keep": "Keep",
        "trash": "Move to trash",
        "keep_key": "Keep (K)",
        "trash_key": "Move to trash (X)",
        "previous": "Previous",
        "next": "Next",
        "swipe_hint": "Swipe right to keep, left to move to trash. Keys: K keeps, X removes.",
        "view_single": "One at a time",
        "view_grid": "Grid",
        "open_immich": "Open in Immich",
        "technical": "Why was this suggested?",
        "taken": "Taken",
        "camera": "Camera",
        "file": "File",
        "loading_photo": "Loading photo…",
        "photo_failed": "The photo could not be loaded.",
        "retry": "Try again",
        "working": "Working…",
        "empty_title": "All done here",
        "empty_text": "There is nothing left to review in this group.",
        "back_home": "Back to overview",
        "best": "Best",
        "favourite": "Favourite",
        "always_kept": "Always kept",
        "will_remove": "Will be removed",
        "choose_keep": "Keep this one instead",
        "group_hint": "The photo marked Best stays. Tap another photo to keep that one instead.",
        "group_trash": "Keep the chosen one, remove the rest",
        "group_stack": "Stack them instead",
        "group_stack_hint": "Stacking removes nothing: Immich shows the group as one photo.",
        "group_keep_all": "Keep all of them",
        "group_of": "Group {i} of {n}",
        "select_all": "Select all",
        "select_none": "Clear selection",
        "selected": "{n} selected",
        "select_photo": "Select {name}",
        "keep_selected": "Keep selected",
        "trash_selected": "Move selected to trash",
        "page_of": "Page {i} of {n}",
        "confirm_title": "Move {n} photos to the trash?",
        "confirm_title_one": "Move 1 photo to the trash?",
        "confirm_text": "They stay in the Immich trash for 30 days and can be brought back.",
        "confirm_yes": "Move {n} to trash",
        "cancel": "Cancel",
        "undo": "Undo",
        "close": "Close",
        "msg_kept": "Kept {n} photos. They will not be suggested again.",
        "msg_kept_one": "Kept. This photo will not be suggested again.",
        "msg_trashed": "Moved {n} photos to the trash. They can be brought back for 30 days.",
        "msg_trashed_one": "Moved to the trash. It can be brought back for 30 days.",
        "msg_trash_dry": "Practice mode: {n} photos would have been moved to the trash. "
        "Nothing was changed.",
        "msg_stacked": "Stacked {n} photos in Immich. Nothing was removed.",
        "msg_stack_dry": "Practice mode: {n} photos would have been stacked. Nothing was changed.",
        "msg_restored": "Brought back {n} photos from the trash.",
        "msg_restored_one": "Brought the photo back from the trash.",
        "msg_restore_dry": "Practice mode: nothing was changed.",
        "msg_keeper": "This photo will be kept instead.",
        "msg_nothing": "Nothing was selected.",
        "msg_stale": "That was already taken care of.",
        "msg_group_changed": "This group changed in Immich. It will be refreshed at the next "
        "check.",
        "msg_immich_error": "Immich could not be reached, so nothing was changed. "
        "Please try again in a moment.",
        "msg_immich_denied": "Immich refused the request, so nothing was changed. "
        "The owner needs to check the curator's API key permissions.",
        "msg_skipped": "{n} photos were left alone because they are now favourites, archived "
        "or already gone.",
        "msg_skipped_one": "1 photo was left alone because it is now a favourite, archived "
        "or already gone.",
        "activity_title": "History",
        "activity_lead": "Everything that was done here, newest first.",
        "activity_empty": "Nothing has been done yet.",
        "act_keep": "Kept {n} photos",
        "act_keep_one": "Kept 1 photo",
        "act_trash": "Moved {n} photos to the trash",
        "act_trash_one": "Moved 1 photo to the trash",
        "act_restore": "Brought {n} photos back from the trash",
        "act_restore_one": "Brought 1 photo back from the trash",
        "act_stack": "Stacked {n} photos",
        "act_set_keeper": "Chose a different photo to keep",
        "act_dry": "Practice mode, nothing changed",
        "act_undone": "Undone",
        "by": "by {user}",
        "err_title": "Something went wrong",
        "err_forbidden": "Please open this page from Drive. It only works for the family's "
        "administrators.",
        "err_notfound": "That page does not exist.",
        "err_generic": "The page could not be shown. Please try again.",
    },
    "vi": {
        "app_name": "Dọn ảnh",
        "nav_home": "Tổng quan",
        "nav_activity": "Lịch sử",
        "nav_drive": "Drive",
        "menu": "Cài đặt",
        "language": "Ngôn ngữ",
        "theme": "Giao diện",
        "theme_auto": "Tự động",
        "theme_light": "Sáng",
        "theme_dark": "Tối",
        "signed_in_as": "Đang đăng nhập: {user}",
        "back": "Quay lại",
        "cat_blurry": "Ảnh bị mờ",
        "cat_dark": "Ảnh tối hoặc trống",
        "cat_screenshot": "Ảnh chụp màn hình",
        "cat_burst": "Ảnh gần giống nhau",
        "cat_duplicate": "Ảnh trùng lặp",
        "help_blurry": "Những ảnh chụp bị nhòe, không rõ nét.",
        "help_dark": "Ảnh gần như đen, trắng hoặc trống, ví dụ chụp nhầm trong túi.",
        "help_screenshot": "Ảnh chụp lại màn hình, không phải chụp bằng máy ảnh.",
        "help_burst": "Nhiều ảnh chụp liền nhau trong vài giây. Giữ lại tấm đẹp nhất.",
        "help_duplicate": "Cùng một ảnh nhưng được lưu nhiều lần (do Immich tìm thấy).",
        "reason_blurry": "Ảnh bị mờ",
        "reason_dark": "Ảnh tối hoặc trống",
        "reason_screenshot": "Ảnh chụp màn hình",
        "reason_burst": "{n} ảnh gần giống nhau",
        "reason_duplicate": "{n} bản của cùng một ảnh",
        "home_title": "Dọn dẹp ảnh",
        "home_lead": "Những ảnh này có thể không cần giữ. Bạn là người quyết định; "
        "không có gì bị bỏ đi nếu bạn chưa đồng ý.",
        "home_count": "Có {n} ảnh cần xem",
        "home_count_one": "Có 1 ảnh cần xem",
        "home_start": "Bắt đầu xem",
        "home_all_done": "Gọn gàng rồi. Hiện không có ảnh nào cần xem.",
        "home_all_done_hint": "Ảnh mới được kiểm tra mỗi đêm.",
        "count_photos": "{n} ảnh",
        "count_photos_one": "1 ảnh",
        "none_left": "Không còn ảnh nào",
        "turned_off": "Đang tắt",
        "safe_note": "Ảnh bạn bỏ đi sẽ nằm trong thùng rác của Immich và có thể lấy lại "
        "trong 30 ngày.",
        "video_note": "Chỉ kiểm tra ảnh. Video không bao giờ bị đề xuất bỏ.",
        "dry_run_note": "Đang ở chế độ thử: không có gì thay đổi trong Immich.",
        "scan_title": "Kiểm tra hằng đêm",
        "scan_never": "Thư viện chưa được kiểm tra lần nào.",
        "scan_running": "Đang kiểm tra.",
        "scan_completed": "Kiểm tra gần nhất: {when}.",
        "scan_interrupted": "Lần kiểm tra trước bị dừng giữa chừng. Lần sau sẽ làm tiếp.",
        "scan_failed": "Lần kiểm tra trước chưa xong. Có thể Immich không kết nối được.",
        "scan_progress": "Đã kiểm tra {done} trên {total} ảnh.",
        "scan_remaining": "Còn {n} ảnh sẽ được kiểm tra trong những đêm tới.",
        "kept_forever": "{n} ảnh đã được đánh dấu giữ lại mãi mãi.",
        "details": "Chi tiết",
        "left": "Còn {n} ảnh",
        "left_one": "Còn 1 ảnh",
        "keep": "Giữ lại",
        "trash": "Bỏ vào thùng rác",
        "keep_key": "Giữ lại (K)",
        "trash_key": "Bỏ vào thùng rác (X)",
        "previous": "Ảnh trước",
        "next": "Ảnh sau",
        "swipe_hint": "Vuốt sang phải để giữ, sang trái để bỏ. Phím: K để giữ, X để bỏ.",
        "view_single": "Từng ảnh",
        "view_grid": "Dạng lưới",
        "open_immich": "Mở trong Immich",
        "technical": "Vì sao ảnh này được đề xuất?",
        "taken": "Ngày chụp",
        "camera": "Máy ảnh",
        "file": "Tệp",
        "loading_photo": "Đang tải ảnh…",
        "photo_failed": "Không tải được ảnh.",
        "retry": "Thử lại",
        "working": "Đang xử lý…",
        "empty_title": "Xong rồi",
        "empty_text": "Không còn ảnh nào cần xem trong nhóm này.",
        "back_home": "Về trang tổng quan",
        "best": "Đẹp nhất",
        "favourite": "Yêu thích",
        "always_kept": "Luôn được giữ",
        "will_remove": "Sẽ bị bỏ",
        "choose_keep": "Giữ ảnh này",
        "group_hint": "Ảnh có nhãn Đẹp nhất sẽ được giữ. Chạm vào ảnh khác nếu bạn muốn "
        "giữ ảnh đó.",
        "group_trash": "Giữ ảnh đã chọn, bỏ phần còn lại",
        "group_stack": "Gom thành một chồng",
        "group_stack_hint": "Gom chồng không bỏ ảnh nào: Immich hiển thị cả nhóm như một ảnh.",
        "group_keep_all": "Giữ tất cả",
        "group_of": "Nhóm {i} trên {n}",
        "select_all": "Chọn tất cả",
        "select_none": "Bỏ chọn",
        "selected": "Đã chọn {n}",
        "select_photo": "Chọn {name}",
        "keep_selected": "Giữ ảnh đã chọn",
        "trash_selected": "Bỏ ảnh đã chọn vào thùng rác",
        "page_of": "Trang {i} trên {n}",
        "confirm_title": "Bỏ {n} ảnh vào thùng rác?",
        "confirm_text": "Ảnh sẽ nằm trong thùng rác của Immich 30 ngày và có thể lấy lại.",
        "confirm_yes": "Bỏ {n} ảnh vào thùng rác",
        "cancel": "Hủy",
        "undo": "Hoàn tác",
        "close": "Đóng",
        "msg_kept": "Đã giữ {n} ảnh. Những ảnh này sẽ không bị đề xuất nữa.",
        "msg_trashed": "Đã bỏ {n} ảnh vào thùng rác. Có thể lấy lại trong 30 ngày.",
        "msg_trash_dry": "Chế độ thử: {n} ảnh lẽ ra đã được bỏ vào thùng rác. "
        "Không có gì thay đổi.",
        "msg_stacked": "Đã gom {n} ảnh thành một chồng trong Immich. Không ảnh nào bị bỏ.",
        "msg_stack_dry": "Chế độ thử: {n} ảnh lẽ ra đã được gom chồng. Không có gì thay đổi.",
        "msg_restored": "Đã lấy lại {n} ảnh từ thùng rác.",
        "msg_restore_dry": "Chế độ thử: không có gì thay đổi.",
        "msg_keeper": "Sẽ giữ ảnh này.",
        "msg_nothing": "Bạn chưa chọn ảnh nào.",
        "msg_stale": "Việc này đã được xử lý rồi.",
        "msg_group_changed": "Nhóm ảnh này đã thay đổi trong Immich. Lần kiểm tra tới sẽ "
        "cập nhật lại.",
        "msg_immich_error": "Không kết nối được tới Immich nên chưa có gì thay đổi. "
        "Vui lòng thử lại sau ít phút.",
        "msg_immich_denied": "Immich từ chối yêu cầu nên chưa có gì thay đổi. "
        "Chủ nhà cần kiểm tra quyền của khóa API.",
        "msg_skipped": "{n} ảnh được giữ nguyên vì đã được yêu thích, lưu trữ hoặc không còn nữa.",
        "activity_title": "Lịch sử",
        "activity_lead": "Mọi việc đã làm ở đây, mới nhất ở trên.",
        "activity_empty": "Chưa có việc nào được thực hiện.",
        "act_keep": "Đã giữ {n} ảnh",
        "act_trash": "Đã bỏ {n} ảnh vào thùng rác",
        "act_restore": "Đã lấy lại {n} ảnh từ thùng rác",
        "act_stack": "Đã gom {n} ảnh thành chồng",
        "act_set_keeper": "Đã chọn ảnh khác để giữ",
        "act_dry": "Chế độ thử, không thay đổi gì",
        "act_undone": "Đã hoàn tác",
        "by": "bởi {user}",
        "err_title": "Có lỗi xảy ra",
        "err_forbidden": "Vui lòng mở trang này từ Drive. Trang chỉ dành cho quản trị viên "
        "của gia đình.",
        "err_notfound": "Trang này không tồn tại.",
        "err_generic": "Không hiển thị được trang. Vui lòng thử lại.",
    },
}

# Strings static/app.js needs (it has no literals of its own).
JS_KEYS = (
    "selected",
    "working",
    "confirm_title",
    "confirm_title_one",
    "confirm_yes",
)

MESSAGE_KEYS = frozenset(k for k in STRINGS["en"] if k.startswith("msg_"))

Translate = Callable[..., str]


def pick_lang(query: str | None, cookie: str | None, accept_language: str | None) -> str:
    """Explicit choice, then the remembered one, then the browser's first language."""
    for choice in (query, cookie):
        if choice in LANGS:
            return str(choice)
    for part in (accept_language or "").split(","):
        tag = part.split(";")[0].strip().lower()
        if not tag:
            continue
        return "vi" if tag.startswith("vi") else "en"
    return DEFAULT_LANG


def translator(lang: str) -> Translate:
    table = STRINGS.get(lang, STRINGS[DEFAULT_LANG])
    fallback = STRINGS[DEFAULT_LANG]

    def t(key: str, **params: object) -> str:
        text = None
        if params.get("n") == 1:
            text = table.get(key + "_one")
        if text is None:
            text = table.get(key)
        if text is None:
            text = fallback.get(key, key)
        return text.format(**params) if params else text

    return t
