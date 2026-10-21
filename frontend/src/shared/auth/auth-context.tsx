import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";

import { apiRequest, ApiError, readStoredKey, readToken, writeStoredKey, writeToken } from "@/shared/api/client";

export type AdminProfile = { username: string; role: string };

type LoginResult = { token: string; username: string; role: string };

type AuthContextValue = {
  admin: AdminProfile | null;
  ready: boolean;
  login: (key: string) => Promise<void>;
  logout: () => Promise<void>;
  changeKey: (currentKey: string, newKey: string) => Promise<void>;
};

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [admin, setAdmin] = useState<AdminProfile | null>(null);
  const [ready, setReady] = useState(false);

  useEffect(() => {
    let cancelled = false;
    const settle = (profile: AdminProfile | null) => {
      if (cancelled) return;
      setAdmin(profile);
      setReady(true);
    };
    async function restore(): Promise<void> {
      const storedKey = readStoredKey();
      if (readToken()) {
        try {
          settle(await apiRequest<AdminProfile>("/admin/api/auth/me"));
          return;
        } catch {
          // Session expired (server restart or 12h TTL) — fall through and try
          // the locally saved credential.
          writeToken("");
        }
      }
      if (storedKey) {
        try {
          const result = await apiRequest<LoginResult>("/admin/api/auth/login", {
            method: "POST",
            body: { key: storedKey },
            auth: false,
          });
          if (!cancelled) {
            writeToken(result.token);
            setAdmin({ username: result.username, role: result.role });
            setReady(true);
          }
          return;
        } catch (error) {
          // Only a 401 from the login endpoint proves the saved key is stale
          // (it was changed on the server). Any other failure — server down,
          // tunnel offline — must keep the credential, or one unreachable
          // moment would erase the very thing this flow exists to preserve.
          if (error instanceof ApiError && error.status === 401) writeStoredKey("");
        }
      }
      settle(null);
    }
    void restore();
    return () => {
      cancelled = true;
    };
  }, []);

  const login = useCallback(async (key: string) => {
    const result = await apiRequest<LoginResult>("/admin/api/auth/login", {
      method: "POST",
      body: { key },
      auth: false,
    });
    writeToken(result.token);
    writeStoredKey(key);
    setAdmin({ username: result.username, role: result.role });
  }, []);

  const logout = useCallback(async () => {
    try {
      await apiRequest("/admin/api/auth/logout", { method: "POST" });
    } catch {
      /* ignore */
    }
    writeToken("");
    writeStoredKey("");
    setAdmin(null);
  }, []);

  const changeKey = useCallback(async (currentKey: string, newKey: string) => {
    await apiRequest("/admin/api/auth/key", {
      method: "POST",
      body: { currentKey, newKey },
    });
    // The server revokes every session on a key change, so log back in with
    // the new key right away instead of dumping the operator at the login
    // panel, and refresh the stored credential to match.
    const result = await apiRequest<LoginResult>("/admin/api/auth/login", {
      method: "POST",
      body: { key: newKey },
      auth: false,
    });
    writeToken(result.token);
    writeStoredKey(newKey);
    setAdmin({ username: result.username, role: result.role });
  }, []);

  const value = useMemo<AuthContextValue>(
    () => ({ admin, ready, login, logout, changeKey }),
    [admin, ready, login, logout, changeKey],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const value = useContext(AuthContext);
  if (!value) throw new Error("useAuth must be used inside AuthProvider");
  return value;
}
