import { useId, useState } from "react";
import { createRoot } from "react-dom/client";
import { Button, DialogActions, Modal } from "./ui.js";

export interface ConfirmActionOptions {
  title?: string;
  confirmLabel?: string;
  /** When set, confirm stays disabled until the user types this exact text. */
  expectedText?: string;
}

// Awaitable modal replacing browser confirm(), which is unstyleable and poor
// for screen readers.
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

    root.render(<ConfirmDialog message={message} options={options} onFinish={finish} />);
  });
}

function ConfirmDialog({
  message,
  options,
  onFinish,
}: {
  message: string;
  options: ConfirmActionOptions;
  onFinish: (confirmed: boolean) => void;
}) {
  const [typed, setTyped] = useState("");
  const inputId = useId();
  const needsTyping = !!options.expectedText;
  const confirmed = !needsTyping || typed === options.expectedText;

  return (
    <Modal title={options.title ?? "Confirm action"} onClose={() => onFinish(false)}>
      <p style={{ margin: 0, color: "var(--color-fg)" }}>{message}</p>
      {needsTyping && (
        <label
          htmlFor={inputId}
          style={{ display: "flex", flexDirection: "column", gap: "0.3rem", marginTop: "0.75rem", fontSize: "0.85rem" }}
        >
          <span>
            Type <strong className="font-mono">{options.expectedText}</strong> to confirm
          </span>
          <input
            id={inputId}
            type="text"
            autoComplete="off"
            spellCheck={false}
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            className="w-full"
            style={{ fontFamily: "var(--font-mono, monospace)" }}
          />
        </label>
      )}
      <DialogActions>
        <Button variant="ghost" onClick={() => onFinish(false)}>
          Cancel
        </Button>
        <Button variant="danger" disabled={!confirmed} onClick={() => onFinish(true)}>
          {options.confirmLabel ?? "Confirm"}
        </Button>
      </DialogActions>
    </Modal>
  );
}
