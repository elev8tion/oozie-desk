document.addEventListener('keydown', (event) => {
  if ((event.metaKey || event.ctrlKey) && event.key === 'Enter') {
    const form = event.target && event.target.closest && event.target.closest('form');
    if (form) {
      event.preventDefault();
      if (window.htmx) window.htmx.trigger(form, 'submit'); else form.requestSubmit();
    }
  }
});

// Apply theme/style changes instantly when the settings selects change;
// the POST persists them for future page loads.
document.addEventListener('change', (event) => {
  const el = event.target;
  if (el && el.name === 'appearance') document.documentElement.dataset.theme = el.value;
  if (el && el.name === 'style_profile') document.documentElement.dataset.style = el.value;
});

document.body.addEventListener('htmx:afterSwap', (event) => {
  const toastRegion = document.getElementById('toast-region');
  const notice = event.detail.target && event.detail.target.querySelector && event.detail.target.querySelector('.notice');
  if (toastRegion && notice && event.detail.target.id !== 'toast-region') {
    const toast = document.createElement('div');
    toast.className = 'toast';
    toast.textContent = notice.textContent;
    toastRegion.replaceChildren(toast);
    setTimeout(() => toast.remove(), 3500);
  }
});
