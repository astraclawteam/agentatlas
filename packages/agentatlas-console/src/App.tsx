// Console entry — the dashboard is self-feeding from the real backends
// (atlas-api :8080 / atlas-agent :8081); paste the admin ticket once.
import { AgentAtlasDashboard } from "./AgentAtlasDashboard";

export default function App() {
  return <AgentAtlasDashboard />;
}
