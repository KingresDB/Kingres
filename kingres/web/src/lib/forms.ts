// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Pressing Enter in a text field submits its form. That is right for a sign-in form and wrong for
// a form whose action changes the whole cluster or cannot be undone: the person pressing Enter in
// a comment field must not apply anything. Put `@keydown.enter="blockEnterSubmit"` on such a form
// and give its action an explicit button.

/** Stops the implicit submission caused by Enter in a single-line field. */
export function blockEnterSubmit(event: KeyboardEvent): void {
  const target = event.target
  if (!(target instanceof HTMLInputElement) && !(target instanceof HTMLSelectElement)) return
  if (target instanceof HTMLInputElement && ['button', 'submit', 'reset'].includes(target.type))
    return
  event.preventDefault()
}

/**
 * `v-block-enter-submit` on a form: the same as `@keydown.enter="blockEnterSubmit"`, as a
 * directive so that the form does not look like an element with an interactive handler.
 */
export const vBlockEnterSubmit = {
  mounted(el: HTMLElement): void {
    el.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') blockEnterSubmit(event)
    })
  },
}
