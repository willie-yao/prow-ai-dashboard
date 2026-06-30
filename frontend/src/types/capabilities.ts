// Capabilities describes the deploy mode the frontend is talking to. The
// Kubernetes-native server publishes it at /api/capabilities; static Pages
// deploys have no such endpoint, so the frontend defaults to read-only static
// mode and interactive features stay off.

export interface CapabilityFeatures {
  chat: boolean;
  actions: boolean;
}

export interface Capabilities {
  mode: "static" | "server";
  features: CapabilityFeatures;
}

// STATIC_CAPABILITIES is the read-only default used whenever no server
// advertises capabilities (the static Pages path).
export const STATIC_CAPABILITIES: Capabilities = {
  mode: "static",
  features: { chat: false, actions: false },
};
