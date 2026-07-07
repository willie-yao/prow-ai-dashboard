import { useState, type ReactNode } from "react";
import { AdminTokenContext } from "../hooks/useAdminToken";

// AdminTokenProvider keeps the admin's GitHub token in memory for the session.
export function AdminTokenProvider({ children }: { children: ReactNode }) {
  const [token, setToken] = useState("");
  return (
    <AdminTokenContext.Provider value={{ token, setToken }}>{children}</AdminTokenContext.Provider>
  );
}
