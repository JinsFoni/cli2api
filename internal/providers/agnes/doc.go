// Package agnes implements the Agnes AI provider adapter (agnes-ai.com).
//
// Agnes is a plain OpenAI-compatible platform: a Bearer API key minted in the
// developer dashboard, POST /v1/chat/completions for generation, GET /v1/models
// for the catalog, and OpenAI chat-completions SSE for streaming. There is no
// OAuth, no refresh token, no check-in, and no usage endpoint — the adapter is
// the "pasted key" shape, mirroring internal/providers/command with far less
// protocol work because the upstream already speaks the gateway's internal
// stream dialect.
package agnes
