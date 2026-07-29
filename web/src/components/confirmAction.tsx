import { createRoot } from "react-dom/client";
import { Button, DialogActions, Modal } from "./ui.js";

export interface ConfirmActionOptions {
  title?: string;
  confirmLabel?: string;
}

// Browser confirm() is synchronous, unstyleable, and inconsistent for screen
// readers. This shared modal preserves focus, traps keyboard navigation, and
// makes destructive confirmation an awaitable application primitive.
export function confirmAction(
  message: string,
  options: ConfirmActionOptions = {},
): Promise<boolean> {
  const host = document.createElement("div");
  host.dataset.confirmAction = "true";
  document.body.append(host);
  const root = createRoot(host);

  return new Promise<boolean>((resolve) => {
    let settled = false;
    const finish = (confirmed: boolean) => {
      if (settled) return;
      settled = true;
      root.unmount();
      host.remove();
      resolve(confirmed);
    };

    root.render(
      <Modal title={options.title ?? "Confirm action"} onClose={() => finish(false)}>
        <p style={{ margin: 0, color: "var(--color-fg)" }}>{message}</p>
        <DialogActions>
          <Button variant="ghost" onClick={() => finish(false)}>
            Cancel
          </Button>
          <Button variant="danger" onClick={() => finish(true)}>
            {options.confirmLabel ?? "Confirm"}
          </Button>
        </DialogActions>
      </Modal>,
    );
  });
}
