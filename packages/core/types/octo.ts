/** An Octo bot installation bound to a single Multica agent.
 *
 * Wire shape mirrors `OctoInstallationResponse` in
 * `server/internal/handler/octo.go`. New backend fields should stay optional
 * so installed desktop builds remain compatible with newer servers. */
export interface OctoInstallation {
  id: string;
  workspace_id: string;
  agent_id: string;
  robot_id: string;
  owner_uid?: string;
  owner_channel_id?: string;
  api_url?: string;
  ws_url?: string;
  installer_user_id: string;
  status: "active" | "revoked" | string;
  installed_at: string;
  created_at: string;
  updated_at: string;
}

export interface ListOctoInstallationsResponse {
  installations: OctoInstallation[];
  configured: boolean;
  install_supported?: boolean;
}

export interface RegisterOctoBYORequest {
  bot_token: string;
  api_url: string;
}

export interface RedeemOctoBindingTokenResponse {
  workspace_id: string;
  installation_id: string;
  octo_user_id: string;
}
