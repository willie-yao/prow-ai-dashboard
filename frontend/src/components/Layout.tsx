import { Link, NavLink, Outlet } from "react-router-dom";
import { SearchBar } from "./SearchBar";

export function Layout() {
  return (
    <div className="min-h-screen bg-background text-on-background">
      <header className="glass sticky top-0 z-50 h-16 flex items-center border-b border-outline-variant px-4 sm:px-6">
        <Link to="/" className="flex items-center gap-3 hover:opacity-80 transition-opacity">
          <svg
            className="h-5 w-5 text-primary"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
          >
            <path d="M21 12V7H5a2 2 0 0 1 0-4h14v4" />
            <path d="M3 5v14a2 2 0 0 0 2 2h16v-5" />
            <path d="M18 12a2 2 0 0 0 0 4h4v-4Z" />
          </svg>
          <h1 className="font-headline text-lg font-semibold tracking-tight text-on-surface hidden sm:block">
            CAPZ Prow Dashboard
          </h1>
        </Link>
        <div className="ml-auto flex items-center gap-4 sm:ml-8 sm:flex-1">
          <SearchBar />
          <nav className="flex items-center gap-4">
            <NavLink
              to="/flaky"
              className={({ isActive }) =>
                `font-label text-sm transition-colors ${
                  isActive
                    ? "text-primary font-medium"
                    : "text-on-surface-variant hover:text-on-surface"
                }`
              }
            >
              Test Analysis
            </NavLink>
          </nav>
        </div>
      </header>

      <main className="mx-auto max-w-7xl px-4 sm:px-6 py-6">
        <Outlet />
      </main>
    </div>
  );
}
