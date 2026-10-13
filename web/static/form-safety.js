'use strict';
(() => {
 const ignoreAttributes = {
  'data-1p-ignore': 'true',
  'data-lpignore': 'true',
  'data-bwignore': 'true',
  'data-form-type': 'other',
 };

 function mark(element) {
  for (const [name, value] of Object.entries(ignoreAttributes)) element.setAttribute(name, value);
 }

 function harden(root = document) {
  const forms = root.matches?.('form') ? [root] : [...root.querySelectorAll?.('form') || []];
  for (const form of forms) {
   if (form.dataset.browserAutofill === 'allow') continue;
   form.autocomplete = 'off';
   mark(form);
  }
  const controls = root.matches?.('input, textarea') ? [root] : [...root.querySelectorAll?.('input, textarea') || []];
  for (const control of controls) {
   if (control.closest('form[data-browser-autofill="allow"]')) continue;
   mark(control);
   if (control.tagName === 'TEXTAREA') control.autocomplete = 'off';
   else if (control.autocomplete === 'one-time-code') {}
   else if (control.type === 'password') control.autocomplete = 'new-password';
   else if (!['button', 'checkbox', 'file', 'hidden', 'radio', 'range', 'reset', 'submit'].includes(control.type)) control.autocomplete = 'off';
  }
 }

 function setFieldGroupEnabled(group, enabled) {
  if (!group) return;
  group.hidden = !enabled;
  for (const control of group.querySelectorAll('button, fieldset, input, select, textarea')) control.disabled = !enabled;
 }

 window.hardenFormTree = harden;
 window.setFieldGroupEnabled = setFieldGroupEnabled;
 const start = () => {
  harden();
  new MutationObserver(records => {
   for (const record of records) for (const node of record.addedNodes) if (node.nodeType === Node.ELEMENT_NODE) harden(node);
  }).observe(document.body, {childList: true, subtree: true});
 };
 if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start, {once: true});
 else start();
})();
