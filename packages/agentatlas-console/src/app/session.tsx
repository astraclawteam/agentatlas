import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";

import { api, SessionRedirect } from "../api/client";

export interface OrgScopeNode {
  id: string;
  name: string;
  children?: OrgScopeNode[];
}

export interface Session {
  authenticated: boolean;
  enterprise_id: string;
  enterprise_name?: string;
  enterprise_user_id: string;
  display_name: string;
  org_version: number;
  org_unit_ids: string[];
  org_tree?: OrgScopeNode[];
  permissions: string[];
  advanced_mode_allowed?: boolean;
  idle_expires_at?: string;
  absolute_expires_at?: string;
}

interface SessionContextValue {
  session: Session;
  advancedMode: boolean;
  setAdvancedMode(enabled: boolean): void;
}

const SessionContext = createContext<SessionContextValue | null>(null);

interface SessionProviderProps {
  children: ReactNode;
  returnTo?: string;
  assignLocation?: (url: string) => void;
}

export function SessionProvider({ children, returnTo, assignLocation }: SessionProviderProps) {
  const [session, setSession] = useState<Session | null>(null);
  const [advancedMode, setAdvancedModeState] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let active = true;
    api<Session>("/api/session", {}, { returnTo, assignLocation })
      .then((value) => {
        if (active) setSession(value);
      })
      .catch((reason: unknown) => {
        if (!active || reason instanceof SessionRedirect) return;
        setError(reason instanceof Error ? reason.message : "会话加载失败");
      });
    return () => {
      active = false;
    };
  }, [assignLocation, returnTo]);

  const value = useMemo<SessionContextValue | null>(() => {
    if (!session) return null;
    return {
      session,
      advancedMode,
      setAdvancedMode(enabled) {
        setAdvancedModeState(Boolean(session.advanced_mode_allowed && enabled));
      },
    };
  }, [advancedMode, session]);

  if (error) {
    return (
      <main className="console-session-state" role="alert">
        <h1>暂时无法打开 AgentAtlas</h1>
        <p>{error}</p>
        <p>请检查网络后刷新页面。</p>
      </main>
    );
  }
  if (!value) {
    return (
      <main className="console-session-state" aria-busy="true" aria-live="polite">
        正在确认登录状态…
      </main>
    );
  }
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionContextValue {
  const value = useContext(SessionContext);
  if (!value) throw new Error("useSession must be used inside SessionProvider");
  return value;
}
