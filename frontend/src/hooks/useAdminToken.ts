import { createContext, useContext } from "react";

// AdminToken holds the admin's GitHub token in memory only. It is never
// persisted, so it clears on reload and has no storage/XSS footprint. Admins
// paste it once per session to enable write actions.
export interface AdminTokenValue {
  token: string;
  setToken: (t: string) => void;
}

export const AdminTokenContext = createContext<AdminTokenValue>({
  token: "",
  setToken: () => {},
});

export function useAdminToken(): AdminTokenValue {
  return useContext(AdminTokenContext);
}
