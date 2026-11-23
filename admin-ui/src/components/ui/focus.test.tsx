import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Button } from './button';
import { Checkbox } from './checkbox';

it('shows keyboard focus on a button and a review checkbox', async () => {
  const user = userEvent.setup();
  render(
    <>
      <Button>Review</Button>
      <Checkbox aria-label="Select book" />
    </>,
  );
  for (const control of [screen.getByRole('button'), screen.getByRole('checkbox')]) {
    await user.tab();
    expect(control).toHaveFocus();
    expect(control).toHaveClass(
      'focus-visible:ring-2',
      'focus-visible:ring-ring',
      'focus-visible:ring-offset-2',
    );
  }
});
