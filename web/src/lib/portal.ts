/** Moves a node to <body>, out of the page's cards. A .card cuts off what it
 *  contains with its clip-path, position: fixed children included: a dialog
 *  drawn inside a card would show only where it overlaps the card. Put the node
 *  inside a wrapper that stays in place (<div class="contents"><div use:portal>…</div></div>):
 *  Svelte removes a component's DOM from its first to its last top-level node,
 *  and neither of them may be one that has moved away. */
export function portal(node: HTMLElement) {
  document.body.appendChild(node);
  return { destroy: () => node.remove() };
}
