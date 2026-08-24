import Link from "next/link";

export default function NotFound() {
  return (
    <main className="flex min-h-screen flex-col items-center justify-center gap-3">
      <h1 className="text-lg font-semibold">Page not found</h1>
      <Link href="/" className="text-sm text-accent hover:underline">
        Back to the overview
      </Link>
    </main>
  );
}
