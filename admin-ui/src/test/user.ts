import type userEvent from '@testing-library/user-event';

/** A click with Shift held (a Shift-click extends a selection). */
export async function shiftClick(user: ReturnType<typeof userEvent.setup>, el: Element) {
  await user.keyboard('{Shift>}');
  await user.click(el);
  await user.keyboard('{/Shift}');
}
