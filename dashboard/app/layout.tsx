import "./globals.css";
import OperatorToken from "@/components/OperatorToken";

export const metadata = {
  description: "Majo operator control plane",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body>
        <nav style={{ padding: "10px 20px", background: "#fff", borderBottom: "1px solid #e5e7eb", display: "flex", gap: "14px", alignItems: "center", flexWrap: "wrap" }}>
          <strong>Majo Ops</strong>
          <a href="/dashboard">Dashboard</a>
          <a href="/requests">Requests</a>
          <a href="/vendors">Vendors</a>
          <a href="/unmatched-responses">Unmatched Responses</a>
          <a href="/offers">Supply Listings</a>
          <a href="/autonomy">Autonomy</a>
          <a href="/discovery">Discovery</a>
          <a href="/events">Events</a>
          <a href="/jobs">Jobs</a>
          <span style={{ marginLeft: "auto" }}>
            <OperatorToken />
          </span>
        </nav>
        <main>{children}</main>
      </body>
    </html>
  );
}
