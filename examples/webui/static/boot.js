import { appearance } from './ui.js';
appearance();
if (window.parent !== window && new URLSearchParams(location.search).has('pane')) {
  document.body.classList.add('pane-page');
  await import('./app.js');
} else {
  document.body.classList.add('hub-page');
  await import('./hub.js');
}
