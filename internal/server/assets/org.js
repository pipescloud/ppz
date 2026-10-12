// Org page: the Pipes tab's create dialog, and async submit for forms
// marked data-async (create, delete pipe, delete source). Errors are shown
// in place instead of navigating to a bare error page; success reloads so
// the table reflects the change.
(function () {
  function showError(form, msg) {
    const el = form.querySelector('[data-form-error]');
    if (el) {
      el.textContent = msg;
      el.hidden = false;
    } else {
      alert(msg);
    }
  }

  const dialog = document.querySelector('[data-create-dialog]');
  const openBtn = document.querySelector('[data-open-create]');
  if (dialog && openBtn) {
    const form = dialog.querySelector('[data-create-form]');
    // Pipe vs source: swap the hint, show the source picker only for
    // pipes, and disable namespace when attaching to a source (the pipe
    // takes the source's namespace).
    const sync = () => {
      const kind = form.elements.kind.value;
      dialog.querySelectorAll('[data-hint]').forEach((el) => {
        el.hidden = el.dataset.hint !== kind;
      });
      dialog.querySelector('[data-field="source"]').hidden = kind !== 'pipe';
      form.elements.namespace.disabled = kind === 'pipe' && form.elements.source.value !== '';
    };
    form.addEventListener('change', sync);
    openBtn.addEventListener('click', () => {
      form.reset();
      form.querySelector('[data-form-error]').hidden = true;
      sync();
      dialog.showModal();
      form.elements.name.focus();
    });
    dialog.querySelector('[data-close-dialog]').addEventListener('click', () => dialog.close());
  }

  document.querySelectorAll('form[data-async]').forEach((form) => {
    form.addEventListener('submit', async (ev) => {
      ev.preventDefault();
      if (form.dataset.confirm && !confirm(form.dataset.confirm)) return;
      const buttons = form.querySelectorAll('button');
      buttons.forEach((b) => { b.disabled = true; });
      try {
        const res = await fetch(form.action, {
          method: 'POST',
          body: new URLSearchParams(new FormData(form)),
          credentials: 'same-origin',
        });
        if (res.ok) {
          location.reload();
          return;
        }
        showError(form, (await res.text()).trim() || res.statusText);
      } catch (err) {
        showError(form, 'Request failed: ' + err.message);
      }
      buttons.forEach((b) => { b.disabled = false; });
    });
  });
})();
