// A tiny History API router. App.svelte intercepts same-origin link
// clicks, so a plain <a href="/staging/3"> navigates in place.
export const route = $state({ path: location.pathname });

export function navigate(path, { replace = false } = {}) {
  if (path === route.path) return;
  if (replace) history.replaceState(null, '', path);
  else history.pushState(null, '', path);
  route.path = path;
  window.scrollTo(0, 0);
}

window.addEventListener('popstate', () => {
  route.path = location.pathname;
});
