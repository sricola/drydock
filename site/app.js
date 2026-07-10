// Landing-page enhancement: reveal-on-scroll + copy buttons. Externalized from
// an inline <script> so the page can ship a strict Content-Security-Policy
// (script-src 'self', no 'unsafe-inline'). The reveal *hidden* state lives
// behind `@media (scripting: enabled)` in the CSS, so if this file fails to
// load the content still shows; this only adds the animation and copy UX.
(function () {
  var io = new IntersectionObserver(function (es) {
    es.forEach(function (e) {
      if (e.isIntersecting) { e.target.classList.add('in'); io.unobserve(e.target); }
    });
  }, { threshold: 0.12, rootMargin: '0px 0px -8% 0px' });
  document.querySelectorAll('.reveal').forEach(function (el, i) {
    if (el.closest('.hero')) el.style.transitionDelay = (i * 0.07) + 's';
    io.observe(el);
  });

  // Hero is always above the fold, so reveal it as soon as the DOM is ready (and
  // again on load) so a slow paint can't leave it blank.
  var showHero = function () {
    document.querySelectorAll('.hero .reveal').forEach(function (el) { el.classList.add('in'); });
  };
  if (document.readyState !== 'loading') showHero();
  else document.addEventListener('DOMContentLoaded', showHero);
  window.addEventListener('load', showHero);

  // copy-button handler. Reads the target <pre> text (comments included) and
  // normalizes the non-breaking spaces used in the markup back to real spaces.
  document.querySelectorAll('.copy[data-copy-target]').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var target = document.querySelector(btn.dataset.copyTarget);
      if (!target) return;
      var text = target.innerText.replace(/ /g, ' ');
      var done = function () {
        btn.classList.add('is-copied');
        var label = btn.querySelector('.copy-label');
        var prev = label ? label.textContent : '';
        if (label) label.textContent = 'copied';
        setTimeout(function () {
          btn.classList.remove('is-copied');
          if (label) label.textContent = prev;
        }, 1500);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function () {});
      } else {
        var ta = document.createElement('textarea');
        ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
        document.body.appendChild(ta); ta.select();
        try { document.execCommand('copy'); done(); } catch (_) {}
        document.body.removeChild(ta);
      }
    });
  });
})();
