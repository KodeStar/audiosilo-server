import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Button } from './button';
import { Checkbox } from './checkbox';
import { Switch } from './switch';

it('shows keyboard focus on a button, a review checkbox and a switch', async () => {
  const user = userEvent.setup();
  render(
    <>
      <Button>Review</Button>
      <Checkbox aria-label="Select book" />
      <Switch aria-label="Include other libraries" />
    </>,
  );
  for (const control of [
    screen.getByRole('button'),
    screen.getByRole('checkbox'),
    screen.getByRole('switch'),
  ]) {
    await user.tab();
    expect(control).toHaveFocus();
    expect(control).toHaveClass(
      'focus-visible:ring-2',
      'focus-visible:ring-ring',
      'focus-visible:ring-offset-2',
    );
  }
});
