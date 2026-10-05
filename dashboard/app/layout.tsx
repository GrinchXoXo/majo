import "./globals.css";

export const metadata = {
  title: "Majo Ops",
  description: "Internal operations dashboard — Phase 0 shell",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body>
        <nav style={{ padding: "12px 20px", borderBottom: "1px solid #ddd", display: "flex", gap: "16px" }}>
          <strong>Majo Ops</strong>
          <a href="/dashboard">Dashboard</a>
          <a href="/requests">Requests</a>
          <a href="/vendors">Vendors</a>
          <a href="/unmatched-responses">Unmatched Responses</a>
          <a href="/offers">Supply Listings</a>
          <a href="/events">Events</a>
        </nav>
        <main style={{ padding: "20px" }}>{children}</main>
      </body>
    </html>
  );
}
