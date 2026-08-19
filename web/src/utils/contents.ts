/** Decode the base64 `content` member of a GitHub contents response (UTF-8). */
export function decodeContentsBase64(b64: string): string {
  const bin = atob(b64.replace(/\s/g, ""));
  const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
  return new TextDecoder().decode(bytes);
}

/** Encode UTF-8 text to base64 for the GitHub contents API `content` member. */
export function encodeContentsBase64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}
