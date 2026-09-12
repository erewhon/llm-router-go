// Connect tab — placeholder from the shell leaf. The "Connect tab" leaf in
// Forge (feature "Dashboard v2") replaces this with the real view.
export default {
  id: "connect",
  label: "Connect",
  mount(root, ctx) {
    root.innerHTML = `<p class="tab-placeholder">Connect — not moved into the new shell yet. The legacy dashboard at <a href="/">/</a> still has everything.</p>`;
  },
  unmount() {},
};
