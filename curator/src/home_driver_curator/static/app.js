// Progressive enhancement only: every action also works as a plain form post.
// No string literals for the user here; text comes from data-strings (see i18n.py).
(function () {
  "use strict";
  var doc = document;
  var root = doc.documentElement;
  var lang = root.lang || "en";
  var strings = {};
  try {
    strings = JSON.parse(doc.body.dataset.strings || "{}");
  } catch (e) {
    strings = {};
  }
  function fill(key, n) {
    var text = (n === 1 && strings[key + "_one"]) || strings[key] || "";
    return text.replace("{n}", new Intl.NumberFormat(lang).format(n));
  }
  function each(selector, fn, scope) {
    Array.prototype.forEach.call((scope || doc).querySelectorAll(selector), fn);
  }

  // --- theme -------------------------------------------------------------------------
  function showTheme() {
    var current = root.dataset.theme || "auto";
    each("[data-set-theme]", function (b) {
      b.setAttribute("aria-pressed", String(b.dataset.setTheme === current));
    });
  }
  each("[data-set-theme]", function (button) {
    button.addEventListener("click", function () {
      var value = button.dataset.setTheme;
      if (value === "auto") delete root.dataset.theme;
      else root.dataset.theme = value;
      try {
        if (value === "auto") localStorage.removeItem("hd-theme");
        else localStorage.setItem("hd-theme", value);
      } catch (e) {
        /* not remembered */
      }
      showTheme();
    });
  });
  showTheme();

  // --- dates in the reader's locale ----------------------------------------------------
  each("[data-ts]", function (el) {
    var date = new Date(el.dataset.ts);
    if (isNaN(date.getTime())) return;
    var opts = { dateStyle: "medium", timeStyle: "short" };
    if (el.dataset.tz) opts.timeZone = el.dataset.tz;
    var text = new Intl.DateTimeFormat(lang, opts).format(date);
    el.textContent = el.dataset.tpl ? el.dataset.tpl.replace("{when}", text) : text;
  });

  // --- toast: one at a time, 5 s, paused while pointed at or focused -----------------
  var toast = doc.querySelector("[data-toast]");
  if (toast) {
    var timer = null;
    var hide = function () {
      toast.classList.add("is-leaving");
      setTimeout(function () {
        toast.hidden = true;
      }, 200);
    };
    var arm = function () {
      clearTimeout(timer);
      timer = setTimeout(hide, 5000);
    };
    var hold = function () {
      clearTimeout(timer);
    };
    toast.addEventListener("mouseenter", hold);
    toast.addEventListener("focusin", hold);
    toast.addEventListener("mouseleave", arm);
    toast.addEventListener("focusout", arm);
    each("[data-toast-close]", function (b) {
      b.addEventListener("click", hide);
    }, toast);
    arm();
    // Drop the one-shot message from the address so a reload does not repeat it.
    try {
      var url = new URL(location.href);
      ["m", "n", "sk", "u"].forEach(function (k) {
        url.searchParams.delete(k);
      });
      history.replaceState(null, "", url.pathname + url.search);
    } catch (e) {
      /* keep the address as is */
    }
  }

  // --- photos: skeleton while loading, clear failure with Retry -----------------------
  each("[data-photo]", function (box) {
    var img = box.querySelector("img");
    if (!img) return;
    var done = function () {
      box.classList.remove("is-loading", "is-failed");
    };
    var failed = function () {
      box.classList.remove("is-loading");
      box.classList.add("is-failed");
    };
    if (img.complete) {
      if (img.naturalWidth) done();
      else failed();
    } else {
      img.addEventListener("load", done);
      img.addEventListener("error", failed);
    }
    each("[data-retry]", function (b) {
      b.addEventListener("click", function () {
        box.classList.remove("is-failed");
        box.classList.add("is-loading");
        img.addEventListener("load", done, { once: true });
        img.addEventListener("error", failed, { once: true });
        var src = img.getAttribute("src");
        img.setAttribute("src", "");
        img.setAttribute("src", src);
      });
    }, box);
  });

  // --- one-at-a-time review: K keeps, X removes, arrows move, swipe on touch ---------
  var review = doc.querySelector("[data-review]");
  if (review) {
    var press = function (id) {
      var el = doc.getElementById(id);
      if (el && !el.disabled) el.click();
    };
    doc.addEventListener("keydown", function (event) {
      if (event.ctrlKey || event.metaKey || event.altKey) return;
      var tag = (event.target.tagName || "").toLowerCase();
      if (tag === "input" || tag === "textarea" || tag === "select") return;
      if (doc.querySelector("dialog[open]")) return;
      var key = event.key.toLowerCase();
      if (key === "k") press("btn-keep");
      else if (key === "x") press("btn-trash");
      else if (key === "arrowleft") press("prev");
      else if (key === "arrowright") press("next");
      else return;
      event.preventDefault();
    });

    var stage = doc.querySelector("[data-swipe]");
    if (stage) {
      var startX = 0;
      var startY = 0;
      var tracking = false;
      stage.addEventListener("touchstart", function (event) {
        if (event.touches.length !== 1) return;
        tracking = true;
        startX = event.touches[0].clientX;
        startY = event.touches[0].clientY;
      }, { passive: true });
      stage.addEventListener("touchmove", function (event) {
        if (!tracking) return;
        var dx = event.touches[0].clientX - startX;
        var dy = event.touches[0].clientY - startY;
        if (Math.abs(dy) > Math.abs(dx)) return;
        stage.style.transform = "translateX(" + dx + "px) rotate(" + dx / 40 + "deg)";
        stage.dataset.lean = dx > 40 ? "keep" : dx < -40 ? "trash" : "";
      }, { passive: true });
      stage.addEventListener("touchend", function (event) {
        if (!tracking) return;
        tracking = false;
        var dx = event.changedTouches[0].clientX - startX;
        var dy = event.changedTouches[0].clientY - startY;
        stage.style.transform = "";
        stage.dataset.lean = "";
        if (Math.abs(dx) < 90 || Math.abs(dy) > Math.abs(dx)) return;
        press(dx > 0 ? "btn-keep" : "btn-trash");
      });
    }
  }

  // --- grid mode: select all, live count, one confirmation for a bulk removal -----------
  var grid = doc.querySelector("[data-grid]");
  if (grid) {
    var boxes = grid.querySelectorAll("input[type=checkbox][name=sel]");
    var count = grid.querySelector("[data-count]");
    var all = grid.querySelector("[data-select-all]");
    var selected = function () {
      return grid.querySelectorAll("input[name=sel]:checked").length;
    };
    var refresh = function () {
      var n = selected();
      if (count) count.textContent = fill("selected", n);
      each("[data-needs-selection]", function (b) {
        b.disabled = n === 0;
      }, grid);
      if (all) {
        var every = n === boxes.length && n > 0;
        all.setAttribute("aria-pressed", String(every));
        all.querySelector(".btn-label").textContent = every ? all.dataset.none : all.dataset.all;
      }
    };
    each("input[name=sel]", function (b) {
      b.addEventListener("change", refresh);
    }, grid);
    if (all) {
      all.addEventListener("click", function () {
        var turnOn = selected() !== boxes.length;
        each("input[name=sel]", function (b) {
          b.checked = turnOn;
        }, grid);
        refresh();
      });
    }
    refresh();

    var dialog = doc.getElementById("confirm");
    var confirmed = grid.querySelector("input[name=confirm]");
    if (dialog && dialog.showModal && confirmed) {
      grid.addEventListener("submit", function (event) {
        var pressed = event.submitter;
        var n = selected();
        if (!pressed || pressed.getAttribute("formaction") !== "trash" || n < 2) return;
        if (confirmed.value === "yes") return;
        event.preventDefault();
        dialog.querySelector("[data-confirm-title]").textContent = fill("confirm_title", n);
        dialog.querySelector("[data-confirm-yes]").textContent = fill("confirm_yes", n);
        dialog.returnValue = "";
        dialog.showModal();
        dialog.addEventListener("close", function () {
          if (dialog.returnValue !== "yes") return;
          confirmed.value = "yes";
          grid.requestSubmit(pressed);
        }, { once: true });
      });
    }
  }

  // --- forms: show that something is happening, prevent double taps -----------------
  // (registered last, so the bulk confirmation above can cancel a submit first)
  each("form[method=post]", function (form) {
    form.addEventListener("submit", function (event) {
      if (event.defaultPrevented) return;
      form.classList.add("is-busy");
      form.setAttribute("aria-busy", "true");
      var pressed = event.submitter;
      if (pressed) {
        var label = pressed.querySelector(".btn-label");
        if (label && strings.working) label.textContent = strings.working;
      }
      // Disable after the browser has collected the form data.
      setTimeout(function () {
        each("button", function (b) {
          b.disabled = true;
        }, form);
      }, 0);
    });
  });
})();
