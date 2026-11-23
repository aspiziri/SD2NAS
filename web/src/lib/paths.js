// match('/staging/:id', '/staging/3') → { id: '3' }; null when path doesn't fit.
export function match(pattern, path) {
  const ps = pattern.split('/');
  const xs = path.split('/');
  if (ps.length !== xs.length) return null;
  const params = {};
  for (let i = 0; i < ps.length; i++) {
    if (ps[i].startsWith(':')) {
      if (xs[i] === '') return null;
      try {
        params[ps[i].slice(1)] = decodeURIComponent(xs[i]);
      } catch {
        return null; // a malformed %-escape is just a path that doesn't match
      }
    } else if (ps[i] !== xs[i]) {
      return null;
    }
  }
  return params;
}

// isId reports whether s is a positive integer without leading zeros.
export function isId(s) {
  return typeof s === 'string' && /^[1-9][0-9]*$/.test(s);
}
