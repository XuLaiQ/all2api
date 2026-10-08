import { createBrowserRouter } from "react-router-dom";
import { AppShell } from "./app/AppShell";
import { AccountsPage } from "./features/accounts/AccountsPage";
import { AuditPage } from "./features/audit/AuditPage";
import { ChannelsPage } from "./features/channels/ChannelsPage";
import { NotFoundPage } from "./app/NotFoundPage";
import { KeysPage } from "./features/keys/KeysPage";
import { LogsPage } from "./features/logs/LogsPage";
import { ModelsPage } from "./features/models/ModelsPage";
import { MediaLibraryPage } from "./features/media/MediaLibraryPage";
import { OverviewPage } from "./features/overview/OverviewPage";
import { RoutesPage } from "./features/routes/RoutesPage";
import { SystemPage } from "./features/system/SystemPage";
import { UsagePage } from "./features/usage/UsagePage";
import { UsersPage } from "./features/management/UsersPage";
import { PlaygroundPage } from "./features/management/PlaygroundPage";

export const router = createBrowserRouter([
  {
    path: "/",
    element: <AppShell />,
    children: [
      { index: true, element: <OverviewPage /> },
      { path: "usage", element: <UsagePage /> },
      { path: "playground", element: <PlaygroundPage /> },
      { path: "users", element: <UsersPage /> },
      { path: "logs", element: <LogsPage /> },
      { path: "audit-logs", element: <AuditPage /> },
      { path: "system", element: <SystemPage /> },
      { path: "keys", element: <KeysPage /> },
      { path: "routes", element: <RoutesPage /> },
      { path: "channels", element: <ChannelsPage /> },
      { path: "accounts", element: <AccountsPage /> },
      { path: "models", element: <ModelsPage /> },
      { path: "media", element: <MediaLibraryPage /> },
      { path: "*", element: <NotFoundPage /> },
    ],
  },
]);
