/* Aryan Jain — portfolio. Three small things: a mobile nav, scroll reveal,
   and a copy-to-clipboard on the contact page. No dependencies. */
(function () {
  "use strict";

  /* ---------- mobile nav ---------- */
  var toggle = document.querySelector(".nav__toggle");
  var links = document.getElementById("nav-links");

  if (toggle && links) {
    var setOpen = function (open) {
      toggle.setAttribute("aria-expanded", open ? "true" : "false");
      links.classList.toggle("is-open", open);
    };

    toggle.addEventListener("click", function () {
      setOpen(toggle.getAttribute("aria-expanded") !== "true");
    });

    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape") setOpen(false);
    });

    document.addEventListener("click", function (e) {
      if (!links.contains(e.target) && !toggle.contains(e.target)) setOpen(false);
    });

    // Drop the open state if the viewport grows past the mobile breakpoint,
    // otherwise the panel is stuck visible over the desktop layout.
    var mq = window.matchMedia("(min-width: 761px)");
    var onChange = function (ev) { if (ev.matches) setOpen(false); };
    if (mq.addEventListener) mq.addEventListener("change", onChange);
    else if (mq.addListener) mq.addListener(onChange);
  }

  /* ---------- scroll reveal ---------- */
  var revealables = document.querySelectorAll(".reveal");
  var reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  if (!("IntersectionObserver" in window) || reduced) {
    for (var i = 0; i < revealables.length; i++) revealables[i].classList.add("is-in");
  } else {
    var io = new IntersectionObserver(
      function (entries) {
        entries.forEach(function (entry) {
          if (!entry.isIntersecting) return;
          entry.target.classList.add("is-in");
          io.unobserve(entry.target);
        });
      },
      { rootMargin: "0px 0px -8% 0px", threshold: 0.08 }
    );

    revealables.forEach(function (el) {
      // Anything already on screen at load reveals immediately, so the
      // first viewport never sits blank.
      if (el.getBoundingClientRect().top < window.innerHeight * 0.92) {
        el.classList.add("is-in");
      } else {
        io.observe(el);
      }
    });
  }

  /* ---------- copy email ---------- */
  var copy = document.querySelector("[data-copy]");
  if (copy) {
    var original = copy.querySelector("[data-copy-label]");
    copy.addEventListener("click", function () {
      var value = copy.getAttribute("data-copy");
      var done = function () {
        copy.classList.add("is-done");
        if (original) original.textContent = "Copied";
        window.setTimeout(function () {
          copy.classList.remove("is-done");
          if (original) original.textContent = "Copy address";
        }, 1800);
      };

      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(value).then(done, function () {});
        return;
      }

      // Older Safari / non-secure contexts.
      var ta = document.createElement("textarea");
      ta.value = value;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); done(); } catch (err) {}
      document.body.removeChild(ta);
    });
  }
})();
