import sodium from "libsodium-wrappers";

/**
 * Seal a secret for the Actions secrets API: crypto_box_seal against the
 * scope's X25519 public key, base64 in and out, per PUT .../secrets/{name}.
 */
export async function sealSecret(plaintext: string, publicKeyB64: string): Promise<string> {
  await sodium.ready;
  // Pass the string straight through: a TextEncoder Uint8Array can fail
  // libsodium's instanceof check across realms (e.g. jsdom).
  const sealed = sodium.crypto_box_seal(
    plaintext,
    sodium.from_base64(publicKeyB64, sodium.base64_variants.ORIGINAL),
  );
  return sodium.to_base64(sealed, sodium.base64_variants.ORIGINAL);
}
