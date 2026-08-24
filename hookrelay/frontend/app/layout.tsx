import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "HookRelay",
  description: "Reliable webhook delivery — signing, retries, dead letters and replay.",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body className="min-h-screen bg-canvas text-ink antialiased">{children}</body>
    </html>
  );
}
