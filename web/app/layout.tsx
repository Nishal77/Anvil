import type { ReactNode } from "react";

export const metadata = {
  title: "Anvil",
  description: "Submit a prompt, watch it build.",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body style={{ fontFamily: "monospace", margin: "2rem" }}>
        {children}
      </body>
    </html>
  );
}
