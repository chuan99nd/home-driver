// Runs before first paint: marks the document as script-enabled and applies a remembered theme.
(function () {
  var root = document.documentElement;
  root.classList.add("js");
  try {
    var theme = localStorage.getItem("hd-theme");
    if (theme === "light" || theme === "dark") root.dataset.theme = theme;
  } catch (e) {
    /* storage unavailable: follow the system theme */
  }
})();
