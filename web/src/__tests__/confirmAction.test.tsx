import { afterEach, describe, expect, it } from "vitest";
import { cleanup, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { confirmAction } from "../components/confirmAction.js";

afterEach(() => {
  cleanup();
  document.querySelectorAll("[data-confirm-action]").forEach((node) => node.remove());
});

describe("confirmAction", () => {
  it("uses a keyboard-accessible modal and resolves cancellation", async () => {
    const result = confirmAction("Delete the repository?");
    const dialog = await screen.findByRole("dialog", { name: "Confirm action" });
    expect(dialog).toHaveAttribute("aria-modal", "true");

    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await expect(result).resolves.toBe(false);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("resolves confirmation and removes the modal host", async () => {
    const result = confirmAction("Remove the collaborator?", {
      title: "Remove collaborator",
      confirmLabel: "Remove",
    });
    await userEvent.click(
      await screen.findByRole("button", { name: "Remove" }),
    );
    await expect(result).resolves.toBe(true);
    expect(document.querySelector("[data-confirm-action]")).toBeNull();
  });

  it("keeps the confirm button disabled until the expected text is typed", async () => {
    const result = confirmAction("Delete the repository?", {
      confirmLabel: "Delete",
      expectedText: "octo/repo",
    });
    const confirm = await screen.findByRole("button", { name: "Delete" });
    expect(confirm).toBeDisabled();

    const input = screen.getByLabelText(/type .* to confirm/i);
    await userEvent.type(input, "octo/nope");
    expect(confirm).toBeDisabled();

    await userEvent.clear(input);
    await userEvent.type(input, "octo/repo");
    expect(confirm).toBeEnabled();

    await userEvent.click(confirm);
    await expect(result).resolves.toBe(true);
    expect(document.querySelector("[data-confirm-action]")).toBeNull();
  });

  it("still cancels a type-to-confirm dialog without typing", async () => {
    const result = confirmAction("Archive?", { expectedText: "repo" });
    await userEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    await expect(result).resolves.toBe(false);
  });
});
