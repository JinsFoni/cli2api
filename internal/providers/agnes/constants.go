package agnes

// Pinned upstream constants. Agnes AI is an OpenAI-compatible API hub: one
// Bearer key, one base URL, standard chat-completions paths. The image and
// video models are served by separate generation endpoints and stay out of
// scope for this chat gateway adapter.
const (
	// CredentialFormat is the canonical stored credential format for
	// provider=agnes.
	CredentialFormat = "agnes-key-v1"

	// BaseURL is the Agnes AI API gateway root, version segment included.
	BaseURL = "https://apihub.agnes-ai.com/v1"

	// PathChatCompletions is the OpenAI-compatible generation endpoint.
	PathChatCompletions = "/chat/completions"

	// PathModels lists the models the key can serve. It doubles as the
	// probe endpoint: no whoami-style profile route exists.
	PathModels = "/models"
)
