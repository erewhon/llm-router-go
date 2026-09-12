// Traffic tab — placeholder from the shell leaf. The "Traffic tab" leaf in
// Forge (feature "Dashboard v2") replaces this with the real view.
export default {
  id: "traffic",
  label: "Traffic",
  mount(root, ctx) {
    root.innerHTML = `<p class="tab-placeholder">Traffic — not moved into the new shell yet. The legacy dashboard at <a href="/">/</a> still has everything.</p>`;
  },
  unmount() {},
};
