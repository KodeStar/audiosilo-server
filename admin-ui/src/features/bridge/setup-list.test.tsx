import { render, screen } from '@testing-library/react';
import { BridgeSetupList } from './setup-list';

// The Bridge's setup checklist as the console shows it (BRIDGE-07: the agent
// step in plain words, saying what it is for).

describe('BridgeSetupList', () => {
  it('says what the agent is for when the step is not done and the Bridge sent no detail', async () => {
    render(
      <BridgeSetupList
        setup={{
          items: [
            { id: 'paired', done: true, required: true },
            { id: 'agent', done: false, required: true },
          ],
          can_contribute: false,
        }}
      />,
    );
    expect(await screen.findByText('Agent credential set')).toBeInTheDocument();
    expect(
      screen.getByText(/The agent writes characters, recaps and descriptions/),
    ).toBeInTheDocument();
  });

  it("shows the Bridge's own words for the step when it sends them", () => {
    render(
      <BridgeSetupList
        setup={{
          items: [{ id: 'agent', done: false, required: true, detail: 'No agent signed in yet.' }],
          can_contribute: false,
        }}
      />,
    );
    expect(screen.getByText('No agent signed in yet.')).toBeInTheDocument();
    expect(screen.queryByText(/The agent writes/)).toBeNull();
  });
});
