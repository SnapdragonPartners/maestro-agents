// Addepar Intake: small enhancements; every page works without them.
"use strict";
document.addEventListener("DOMContentLoaded", () => {
  // Hovering or focusing a value emphasizes its boxes on the page.
  const rows = document.querySelectorAll("tr[data-field]");
  const marks = (f) => document.querySelectorAll(`rect[data-field="${CSS.escape(f)}"]`);
  rows.forEach((row) => {
    const f = row.dataset.field;
    const on = () => marks(f).forEach((m) => m.classList.add("on"));
    const off = () => marks(f).forEach((m) => m.classList.remove("on"));
    row.addEventListener("mouseenter", on);
    row.addEventListener("mouseleave", off);
    row.addEventListener("focus", on);
    row.addEventListener("blur", off);
    row.addEventListener("click", () => {
      const m = marks(f)[0];
      if (m) m.closest("figure").scrollIntoView({ behavior: "smooth", block: "start" });
    });
  });
  // Setup: choosing a provider fills its default model.
  const provider = document.querySelector('select[name="provider"]');
  const model = document.querySelector('input[name="model"]');
  if (provider && model) {
    // A model that is some provider's default is the page's own choice,
    // replaced when the provider changes; anything else the person typed.
    const defaults = Array.from(provider.options, (o) => o.dataset.model);
    model.dataset.auto = !model.value || defaults.includes(model.value) ? "1" : "0";
    const fill = () => {
      const opt = provider.selectedOptions[0];
      if (opt && model.dataset.auto === "1") {
        model.value = opt.dataset.model;
        model.dataset.auto = "1";
      }
    };
    model.addEventListener("input", () => { model.dataset.auto = "0"; });
    provider.addEventListener("change", fill);
    fill();
  }
});
