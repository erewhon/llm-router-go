// Fleet tab — placeholder from the shell leaf. The "Fleet tab" leaf in
// Forge (feature "Dashboard v2") replaces this with the real view.
export default {
  id: "fleet",
  label: "Fleet",
  mount(root, ctx) {
    root.innerHTML = `<p class="tab-placeholder">Fleet — not moved into the new shell yet. The legacy dashboard at <a href="/">/</a> still has everything.</p>`;
  },
  unmount() {},
};
