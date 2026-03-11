package xai

var ModelList = []string{
	// grok-4.20 multi-agent (Responses API only)
	"grok-4.20-multi-agent-beta-0309",
	// grok-4.20 reasoning / non-reasoning
	"grok-4.20-beta-0309-reasoning", "grok-4.20-beta-0309-non-reasoning",
	"grok-4.20-beta-latest-reasoning", "grok-4.20-beta-latest-non-reasoning",
	// grok-4-1-fast
	"grok-4-1-fast-reasoning", "grok-4-1-fast-non-reasoning",
	// grok-code
	"grok-code-fast-1",
	// grok-4
	"grok-4", "grok-4-0709", "grok-4-0709-search",
	// grok-3
	"grok-3-beta", "grok-3-mini-beta",
	// grok-3 mini
	"grok-3-fast-beta", "grok-3-mini-fast-beta",
	// extend grok-3-mini reasoning
	"grok-3-mini-beta-high", "grok-3-mini-beta-low", "grok-3-mini-beta-medium",
	"grok-3-mini-fast-beta-high", "grok-3-mini-fast-beta-low", "grok-3-mini-fast-beta-medium",
	// image model
	"grok-2-image",
	"grok-imagine-image",
	"grok-imagine-image-pro",
	// video model
	"grok-imagine-video",
	// legacy models
	"grok-2", "grok-2-vision",
	"grok-beta", "grok-vision-beta",
}

var ChannelName = "xai"
