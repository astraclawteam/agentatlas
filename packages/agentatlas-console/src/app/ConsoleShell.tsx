import { useRef, useState } from "react";
import { Button, DesignProvider, DockedPanel } from "@xiaozhiclaw/runtime-ui";
import { Building2, ChevronDown, Sparkles, X } from "lucide-react";
import {
  BrowserRouter,
  MemoryRouter,
  NavLink,
  useLocation,
  useNavigate,
} from "react-router-dom";

import { ConsoleRoutes, consoleSurfaces } from "./routes";
import { SessionProvider, useSession, type OrgScopeNode } from "./session";
import "./ConsoleShell.css";

export interface ConsoleShellProps {
  initialPath?: string;
  assignLocation?: (url: string) => void;
}

export function ConsoleShell({ initialPath, assignLocation }: ConsoleShellProps) {
  const Router = initialPath ? MemoryRouter : BrowserRouter;
  const routerProps = initialPath ? { initialEntries: [initialPath] } : {};
  const returnTo = initialPath ?? `${window.location.pathname}${window.location.search}`;

  return (
    <DesignProvider theme="light" accent="clay">
      <SessionProvider returnTo={returnTo} assignLocation={assignLocation}>
        <Router {...routerProps}>
          <AuthenticatedShell />
        </Router>
      </SessionProvider>
    </DesignProvider>
  );
}

function AuthenticatedShell() {
  const { session, advancedMode, setAdvancedMode } = useSession();
  const [assistantOpen, setAssistantOpen] = useState(false);
  const agentButtonRef = useRef<HTMLButtonElement>(null);
  const navigate = useNavigate();
  const location = useLocation();
  const isAssistantRoute = location.pathname === "/assistant";

  const openAssistant = () => {
    if (typeof window.matchMedia === "function" && window.matchMedia("(max-width: 900px)").matches) {
      navigate("/assistant");
      return;
    }
    setAssistantOpen(true);
  };

  return (
    <div className="console-root">
      <header className="console-header glass-rest">
        <div className="console-brand" aria-label="AgentAtlas">
          AgentAtlas
        </div>
        <nav className="console-primary-nav" aria-label="主要工作区">
          {consoleSurfaces.map(({ path, label, icon: Icon }) => (
            <NavLink key={path} to={path} className="console-nav-link">
              <Icon aria-hidden size={18} strokeWidth={1.8} />
              <span>{label}</span>
            </NavLink>
          ))}
        </nav>
        <div
          className="console-user-actions items-center whitespace-nowrap"
          data-testid="header-user-actions"
          data-control-height="48"
        >
          <Button
            ref={agentButtonRef}
            className="console-agent-button"
            aria-label="打开 Atlas 助手"
            aria-expanded={assistantOpen || isAssistantRoute}
            aria-controls="atlas-assistant"
            onClick={openAssistant}
          >
            <Sparkles aria-hidden size={20} strokeWidth={1.8} />
          </Button>
          <div className="console-user-group">
            {session.advanced_mode_allowed ? (
              <label className="console-advanced-toggle">
                <input
                  type="checkbox"
                  checked={advancedMode}
                  onChange={(event) => setAdvancedMode(event.currentTarget.checked)}
                />
                高级维护模式
              </label>
            ) : null}
            <span className="console-user-name">
              {session.display_name}
              <ChevronDown aria-hidden size={16} />
            </span>
          </div>
        </div>
      </header>

      <div
        className="console-layout"
        data-testid="console-content-layout"
        data-assistant-open={assistantOpen ? "true" : "false"}
      >
        <OrgScopeNavigation orgTree={session.org_tree} orgUnitIds={session.org_unit_ids} />
        <main className="console-main">
          <ConsoleRoutes />
        </main>
        <div id="atlas-assistant" className="console-assistant-dock">
          <DockedPanel
            open={assistantOpen}
            title="Atlas 助手"
            onOpenChange={setAssistantOpen}
            returnFocusRef={agentButtonRef}
          >
            <div className="console-assistant-copy">
              <p>告诉我你想补充、修正或整理什么，我会带你完成下一步。</p>
              <Button className="console-assistant-close" onClick={() => setAssistantOpen(false)}>
                <X aria-hidden size={17} />
                关闭助手
              </Button>
            </div>
          </DockedPanel>
        </div>
      </div>
    </div>
  );
}

function OrgScopeNavigation({
  orgTree,
  orgUnitIds,
}: {
  orgTree?: OrgScopeNode[];
  orgUnitIds: string[];
}) {
  const allowed = new Set(orgUnitIds);
  const nodes = orgTree?.length
    ? filterAuthorizedNodes(orgTree, allowed)
    : orgUnitIds.map((id) => ({ id, name: id, children: [] }));

  return (
    <nav className="console-org-nav" aria-label="知识范围">
      <p className="console-org-heading">知识范围</p>
      <OrgScopeList nodes={nodes} />
    </nav>
  );
}

function OrgScopeList({ nodes }: { nodes: OrgScopeNode[] }) {
  return (
    <ul className="console-org-list">
      {nodes.map((node) => (
        <li key={node.id}>
          <NavLink className="console-org-link" to={`/knowledge/${encodeURIComponent(node.id)}`}>
            <Building2 aria-hidden size={16} strokeWidth={1.8} />
            {node.name}
          </NavLink>
          {node.children?.length ? <OrgScopeList nodes={node.children} /> : null}
        </li>
      ))}
    </ul>
  );
}

function filterAuthorizedNodes(nodes: OrgScopeNode[], allowed: Set<string>): OrgScopeNode[] {
  return nodes.flatMap((node) => {
    const children = filterAuthorizedNodes(node.children ?? [], allowed);
    if (!allowed.has(node.id) && children.length === 0) return [];
    return [{ ...node, children }];
  });
}
