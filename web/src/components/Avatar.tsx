// Avatar image, falling back to a monogram tile from the login's first letter.
export function Avatar({
  login,
  src,
  size = 40,
  square = false,
}: {
  login: string;
  src?: string | null | undefined;
  size?: number;
  square?: boolean;
}) {
  const [failedSource, setFailedSource] = useState<string | null>(null);
  const radius = square ? "var(--radius-md)" : "50%";
  if (src && src !== failedSource) {
    return (
      <img
        src={src}
        alt=""
        width={size}
        height={size}
        loading="lazy"
        referrerPolicy="no-referrer"
        onError={() => setFailedSource(src)}
        style={{
          width: size,
          height: size,
          borderRadius: radius,
          objectFit: "cover",
          border: "1px solid var(--color-border)",
          flexShrink: 0,
          background: "var(--color-bg-subtle)",
        }}
      />
    );
  }
  return (
    <span
      aria-hidden
      className="inline-flex items-center justify-center"
      style={{
        width: size,
        height: size,
        borderRadius: radius,
        border: "1px solid var(--color-border)",
        background: "var(--color-bg-subtle)",
        color: "var(--color-fg-muted)",
        fontWeight: 600,
        fontSize: size * 0.42,
        flexShrink: 0,
        textTransform: "uppercase",
      }}
    >
      {(login || "?").charAt(0)}
    </span>
  );
}
import { useState } from "react";
