// Catalog tab — placeholder from the shell leaf. The "Catalog tab" leaf in
// Forge (feature "Dashboard v2") replaces this with the real view.
export default {
  id: "catalog",
  label: "Catalog",
  mount(root, ctx) {
    root.innerHTML = `<p class="tab-placeholder">Catalog — not moved into the new shell yet. The legacy dashboard at <a href="/">/</a> still has everything.</p>`;
  },
  unmount() {},
};
