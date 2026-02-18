# Personal Agent — Design & Requirements

**Project:** Codename `kai-go` (working title)
**Owner:** Nate / Zette LLC
**Status:** Pre-development
**Last Updated:** February 2026

-----

## 1. Vision

A personal AI agent that lives on a Mac mini, accessible via Telegram, and powered by Claude Code CLI as its execution engine. The agent is deeply personalized, structurally organized across multiple focused chat threads, capable of writing to Obsidian, and able to store and query structured personal data over time. It is built as a single compiled Go binary — fast, portable, and easy to run as a launchd service.

The core insight driving this project: Claude Code CLI is not just a coding tool. With the right context and workspace setup, it is a capable general-purpose agent. This project treats Claude Code as the processor and builds everything else — session lifecycle, memory, structured data, Telegram routing — around it in Go.

Claude Code is the default executor, but the system is designed around a clean `Executor` interface so other CLI-based agents (Codex, OpenCode, etc.) can be swapped in per-workspace or per-chat without touching the rest of the system.

-----

## 2. Goals

- Route multiple Telegram chats to isolated workspaces, each with its own focused memory
- Maintain a global memory layer for cross-chat context (preferences, facts, identity)
- Keep Claude Code sessions alive across a conversation, expiring after inactivity
- Extract and persist meaningful memories when sessions close
- Write structured notes and documentation to an Obsidian vault
- Store structured personal data (workouts, meals, etc.) in SQLite with queryable schemas
- Run as a secure, locked-down local service with no data leaving the Mac mini
- Learn the Claude Code subprocess model deeply — this is a Zette capability investment

-----

## 3. Non-Goals (v1)

- Multi-user support
- Cloud hosting or remote deployment
- Voice input/output (considered for v2)
- Web UI or dashboard
- MCP server integrations (considered for v2)

-----

## 4. Architecture Overview

```
Telegram App
     │
     ▼
Telegram Bot API
     │
     ▼
┌─────────────────────────────────────────────────┐
│               Go Binary (agent)                  │
│                                                   │
│  ┌──────────┐   ┌───────────────┐   ┌─────────┐ │
│  │ Telegram │   │ Session       │   │ Memory  │ │
│  │ Handler  │──▶│ Manager       │──▶│ Store   │ │
│  └──────────┘   └──────┬────────┘   └─────────┘ │
│                        │                          │
│                        ▼                          │
│                ┌───────────────┐                  │
│                │   Executor    │                  │
│                │  (interface)  │                  │
│                └──────┬────────┘                  │
│                       │                           │
│          ┌────────────┼────────────┐             │
│          ▼            ▼            ▼             │
│   ┌────────────┐ ┌─────────┐ ┌─────────┐        │
│   │ClaudeExec  │ │CodexExec│ │OpenCode │  ...    │
│   └────────────┘ └─────────┘ │  Exec   │        │
│                               └─────────┘        │
│                                                   │
│            ┌──────────┬──────────┐               │
│            ▼          ▼          ▼               │
│     ┌─────────────┐    ┌──────────────────┐      │
│     │  SQLite DB  │    │  Workspace FS    │      │
│     │  (memory,   │    │  (CLAUDE.md,     │      │
│     │   data)     │    │   Obsidian,      │      │
│     └─────────────┘    │   JSONL logs)    │      │
│                         └──────────────────┘      │
└─────────────────────────────────────────────────┘
     │
     ▼
CLI process (per active session — Claude Code, Codex, OpenCode, etc.)
```

-----

## 5. Core Components

### 5.1 Telegram Handler

Receives messages from the Telegram Bot API via long polling. Responsible for:

- Authenticating incoming messages against a whitelist of allowed Telegram user IDs
- Routing messages to the correct session based on chat ID
- Streaming Claude’s response back to Telegram, editing the message in place every ~2 seconds
- Splitting responses that exceed Telegram’s 4096 character limit
- Handling slash commands (`/new`, `/status`, `/workspace`, `/help`)

### 5.2 Session Manager

The heart of the system. Maps Telegram chat IDs to active Claude Code sessions and manages their lifecycle.

**Session lifecycle:**

1. Message arrives for a chat ID
1. If no active session exists, create one (spawn subprocess, inject context)
1. Forward message to the Claude subprocess stdin
1. Stream response back via stdout
1. Reset the inactivity timer on every message
1. After 10 minutes of inactivity, trigger session close:
- Ask Claude to summarize what’s worth persisting
- Write new memories to SQLite
- Kill the subprocess
- Clear session state

**Per-chat lock:** Each chat has an async lock. If a second message arrives while Claude is still responding, it queues and waits. No concurrent Claude requests per chat.

**Session state (in-memory):**

```go
type Session struct {
    ChatID       int64
    Workspace    string
    Cmd          *exec.Cmd
    Stdin        io.WriteCloser
    Stdout       io.Reader
    LastActivity time.Time
    Timer        *time.Timer
    Mu           sync.Mutex
}
```

### 5.3 Executor Interface

The executor is the abstraction layer between the session manager and whatever CLI agent is actually running. All session management, memory, and Telegram code talks only to this interface — never directly to Claude Code or any other tool.

```go
// Executor is the interface any CLI-based agent must implement.
// The session manager only ever talks to this interface.
type Executor interface {
    // Start spawns the underlying process in the given working directory.
    Start(ctx context.Context, workDir string, sessionCtx SessionContext) error

    // Send writes a message and returns a channel of streaming events.
    // The channel closes when the response is complete.
    Send(ctx context.Context, message string) (<-chan Event, error)

    // Stop gracefully shuts down the process.
    Stop() error

    // Alive reports whether the process is still running.
    Alive() bool

    // Name returns a human-readable identifier ("claude", "codex", etc.)
    Name() string
}

// Event is a unit of streamed output from the executor.
type Event struct {
    Type  EventType // Text | Done | Error
    Text  string    // Partial text (for Text events)
    Error error     // Set for Error events
}

// SessionContext is executor-agnostic context the session manager
// builds from memory and history. Each executor materializes this
// into whatever its underlying agent expects (CLAUDE.md, AGENTS.md, etc.)
type SessionContext struct {
    GlobalBriefing string
    ChatMemory     string
    RecentHistory  string
    WorkspaceInfo  string
    IdentityDoc    string
}
```

**Why this boundary matters:** The streaming format, CLI flags, and stdin/stdout protocol differ between agents. Claude Code uses `--output-format stream-json`. Codex has its own interface. OpenCode differs again. All of that complexity lives inside the concrete executor implementation. Nothing outside `internal/executor/` ever touches a subprocess directly.

**Concrete implementations:**

```
internal/executor/
├── executor.go          # Interface, Event, SessionContext definitions
├── claude/
│   └── claude.go        # Spawns claude CLI, parses stream-json, writes CLAUDE.md
├── codex/
│   └── codex.go         # Spawns codex CLI, handles its protocol, writes AGENTS.md
├── opencode/
│   └── opencode.go      # Spawns opencode CLI, handles its protocol
└── mock/
    └── mock.go          # MockExecutor for testing
```

**Per-workspace executor config:** Each workspace specifies which executor to use, letting different chats run different agents from the same binary.

```yaml
workspaces:
  chat_map:
    "-1001234567890":
      name: zette
      executor: claude       # default
    "-1009876543210":
      name: research
      executor: opencode
```

**Session struct holds an Executor, not a concrete subprocess:**

```go
type Session struct {
    ChatID       int64
    Workspace    string
    Executor     executor.Executor   // interface — could be Claude, Codex, etc.
    LastActivity time.Time
    Timer        *time.Timer
    Mu           sync.Mutex
}
```

**Context injection (built by session manager, passed to `Start()`):**

```
[GLOBAL MEMORY BRIEFING]
...synthesized from SQLite global memories...

[CHAT MEMORY]
...memories scoped to this chat ID...

[RECENT HISTORY]
...last 20 messages from JSONL logs...

[WORKSPACE INFO]
Current workspace: <n>
Obsidian vault: <path>
Available data schemas: workouts, meals, ...
Active executor: claude
```

### 5.4 Memory Store

SQLite database (`agent.db`) with two concerns: episodic memory and structured personal data.

**Memory schema:**

```sql
CREATE TABLE memories (
    id          TEXT PRIMARY KEY,
    type        TEXT NOT NULL,     -- fact | preference | decision | goal | event | todo
    content     TEXT NOT NULL,
    scope       TEXT NOT NULL,     -- 'global' or '<chat_id>'
    importance  REAL DEFAULT 1.0,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    accessed_at DATETIME,
    access_count INTEGER DEFAULT 0
);

CREATE VIRTUAL TABLE memories_fts USING fts5(
    content,
    content=memories,
    content_rowid=rowid
);
```

**Memory types:**

- `fact` — objective information about Nate or his world (“lives in Raleigh, NC”)
- `preference` — stylistic or workflow preferences (“prefers concise responses”)
- `decision` — recorded decisions and their rationale (“chose SQLite over Postgres for agent.db”)
- `goal` — active goals or intentions (“building Zette to $50k MRR”)
- `event` — notable things that happened (“launched CarePics integration”)
- `todo` — pending tasks surfaced during conversation

**Memory scopes:**

- `global` — visible in all chat sessions via briefing
- `<chat_id>` — visible only in the relevant chat’s session

**Cortex briefing:** A goroutine runs every 30 minutes, queries all global memories grouped by type, and generates a fresh `BRIEFING.md` in the home workspace. This file is what gets injected into session startup — synthesized context, not raw dumps.

**Conversation history:** JSONL files in `workspaces/home/.logs/<YYYY-MM-DD>.jsonl`. One entry per message, both user and assistant. Scoped by chat ID within each file. The last 20 messages for a given chat are loaded at session start.

### 5.5 Workspace Manager

Maps chat IDs to filesystem directories. Each workspace contains:

```
workspaces/
├── home/                    # Global identity and memory
│   ├── CLAUDE.md            # Identity, instructions, tool guidance
│   ├── BRIEFING.md          # Auto-generated memory briefing (30 min refresh)
│   └── .logs/               # JSONL conversation history
├── zette/                   # Zette LLC work context
│   ├── CLAUDE.md            # Workspace-specific instructions
│   └── .logs/
├── fitness/                 # Workout and meal logging
│   ├── CLAUDE.md
│   └── .logs/
├── personal/                # Personal planning, misc
│   ├── CLAUDE.md
│   └── .logs/
└── obsidian -> /path/to/vault  # Symlink or configured path
```

**Chat-to-workspace mapping** is configured in `config.yaml`. A Telegram chat ID maps to a named workspace. The workspace directory is the Claude Code working directory for that session.

### 5.6 Obsidian Integration

Claude Code has native file write access within its workspace. The Obsidian vault path is exposed to Claude via the `CLAUDE.md` context. No special integration code is needed — Claude writes markdown files directly to the vault path using its built-in file tools.

The `CLAUDE.md` for relevant workspaces includes instructions like:

```
Obsidian vault: /Users/nate/Documents/Obsidian/Main
When asked to document something, create or update a note in the vault.
Use the folder structure: /Areas for ongoing topics, /Projects for active work.
```

### 5.7 Structured Data Layer

Personal data schemas stored in SQLite alongside memories. Claude Code interacts with these via shell commands (`sqlite3`) that it can invoke as tools.

**Initial schemas:**

```sql
CREATE TABLE workouts (
    id          TEXT PRIMARY KEY,
    date        DATE NOT NULL,
    type        TEXT,           -- run | lift | walk | etc
    duration_min INTEGER,
    notes       TEXT,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE meals (
    id          TEXT PRIMARY KEY,
    date        DATE NOT NULL,
    meal_type   TEXT,           -- breakfast | lunch | dinner | snack
    description TEXT,
    calories    INTEGER,
    notes       TEXT,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

New schemas are created on-demand during conversation (“I want to start tracking X”). Claude Code generates the `CREATE TABLE` statement, executes it via shell, and the schema is live. The workspace `CLAUDE.md` is updated to document available schemas so future sessions know what exists.

-----

## 6. Memory System In Detail

The memory system has three layers, inspired by Kai’s architecture with Spacebot’s typed memory categories:

### Layer 1: Auto-memory (managed by Claude Code)

Claude Code natively maintains memory in `~/.claude/projects/`. This is free — no code required. It captures project-level patterns and architecture notes automatically.

### Layer 2: Semantic memory (managed by agent)

The typed SQLite memory store described in 5.4. The Go binary manages writes (extracted at session close) and reads (injected at session start via briefing). Claude can also explicitly request memory writes during a session by calling a shell helper script.

### Layer 3: Episodic memory (conversation logs)

JSONL logs per chat per day. Loaded on session start to provide recent conversational continuity. Full history is searchable on demand via grep/sqlite.

### Memory extraction at session close

When a session times out, the session manager sends a final prompt to Claude before killing the process:

```
Before this session closes, review our conversation and identify anything worth 
remembering long-term. For each item, output a JSON object on its own line:
{"type": "fact|preference|decision|goal|event|todo", "content": "...", "scope": "global|<chat_id>"}
Output only these JSON lines, nothing else.
```

The Go binary parses the output and writes new memories to SQLite.

-----

## 7. Configuration

`config.yaml` in the binary’s working directory:

```yaml
telegram:
  bot_token: "${TELEGRAM_BOT_TOKEN}"
  allowed_user_ids:
    - 123456789

session:
  inactivity_timeout: 10m
  max_response_length: 4096
  edit_interval: 2s

claude:
  model: sonnet           # opus | sonnet | haiku
  max_budget_usd: 10.0    # runaway prevention per session

workspaces:
  base_path: /Users/nate/agent/workspaces
  chat_map:
    "-1001234567890": zette
    "-1009876543210": fitness
    "-1005555555555": personal
  default: home

obsidian:
  vault_path: /Users/nate/Documents/Obsidian/Main

memory:
  db_path: /Users/nate/agent/agent.db
  briefing_interval: 30m
  history_messages: 20

security:
  workspace_base_strict: true   # reject any path traversal outside base_path
```

Secrets (bot token) are loaded from environment variables, not stored in config.

-----

## 8. Security

- **Telegram auth:** All incoming messages checked against `allowed_user_ids`. Unauthorized messages are silently dropped, not acknowledged.
- **Workspace sandboxing:** Claude Code subprocess launched with working directory set to the chat’s workspace. Path traversal guard in Go ensures no workspace resolves outside `base_path`.
- **No inbound network exposure:** No webhook server, no open ports. Long polling only.
- **Credential storage:** Telegram bot token stored in macOS Keychain or environment variable, never in config files committed to git.
- **Local only:** All data stays on the Mac mini. No external memory services, no cloud databases.
- **Process isolation:** Each Claude session is a separate subprocess. Session death doesn’t affect others.

-----

## 9. Go Project Structure

```
agent/
├── cmd/
│   └── agent/
│       └── main.go             # Entry point, wires everything together
├── internal/
│   ├── bot/
│   │   └── bot.go              # Telegram handler, message routing, streaming
│   ├── session/
│   │   ├── manager.go          # Session lifecycle, chat ID → session mapping
│   │   └── session.go          # Session struct, timer management
│   ├── executor/
│   │   ├── executor.go         # Executor interface, Event, SessionContext types
│   │   ├── claude/
│   │   │   └── claude.go       # ClaudeExecutor — claude CLI, stream-json, CLAUDE.md
│   │   ├── codex/
│   │   │   └── codex.go        # CodexExecutor — codex CLI, AGENTS.md
│   │   ├── opencode/
│   │   │   └── opencode.go     # OpenCodeExecutor — opencode CLI
│   │   └── mock/
│   │       └── mock.go         # MockExecutor for testing
│   ├── memory/
│   │   ├── store.go            # SQLite read/write for memories
│   │   ├── history.go          # JSONL conversation log read/write
│   │   └── briefing.go         # Cortex briefing generation goroutine
│   ├── workspace/
│   │   └── manager.go          # Chat ID → workspace path resolution
│   └── config/
│       └── config.go           # YAML config loading, validation
├── workspaces/                 # Runtime workspace directories (gitignored)
├── config.yaml
├── go.mod
└── go.sum
```

**Key dependencies:**

- `github.com/go-telegram-bot-api/telegram-bot-api/v5` — Telegram
- `modernc.org/sqlite` — Pure Go SQLite (no CGo, truly self-contained binary)
- `gopkg.in/yaml.v3` — Config parsing

-----

## 10. Launchd Service (macOS)

`~/Library/LaunchAgents/com.nate.agent.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.nate.agent</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/agent</string>
    </array>
    <key>WorkingDirectory</key>
    <string>/Users/nate/agent</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>TELEGRAM_BOT_TOKEN</key>
        <string><!-- load from keychain in wrapper script --></string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>NetworkState</key>
        <true/>
    </dict>
    <key>StandardOutPath</key>
    <string>/Users/nate/agent/logs/agent.log</string>
    <key>StandardErrorPath</key>
    <string>/Users/nate/agent/logs/agent.error.log</string>
</dict>
</plist>
```

-----

## 11. CLAUDE.md — Home Workspace

The home workspace `CLAUDE.md` is the agent’s identity document. It’s injected into every session regardless of which workspace is active. Example structure:

```markdown
# Identity

You are a personal AI assistant running locally on Nate's Mac mini.
You have access to shell, files, and the web. You remember things across
conversations using a memory system managed by the agent binary.

# About Nate

- Runs Zette LLC, an AI-accelerated software development agency
- Based in Raleigh, NC
- Married to Victoria (artist, charcoal and hand portraiture)
- Two Cirneco dell'Etna dogs: Joey and August
- Deep interest in software architecture, healthcare tech, construction, investing

# Communication Style

- Concise and direct
- Technical depth is welcome — don't over-explain basics
- Be proactive about surfacing relevant context from memory
- If something seems worth remembering, say so

# Memory System

When you identify something worth persisting across sessions, you can write
it explicitly by running:
  echo '{"type":"fact","content":"...","scope":"global"}' >> /tmp/memory_queue.jsonl

Available memory types: fact, preference, decision, goal, event, todo
Scopes: global (all chats) or the current chat_id for chat-specific memory

# Tools Available

- Shell access via bash
- File read/write (workspace and Obsidian vault)
- Web search (built into Claude Code)
- sqlite3 for querying personal data

# Obsidian Vault

Path: /Users/nate/Documents/Obsidian/Main
Folder conventions:
  /Areas    — ongoing life areas (health, finances, home)
  /Projects — active projects (Zette, home renovation)
  /Log      — daily notes and logs
```

-----

## 12. Build Phases

### Phase 1 — Core (MVP)

Get a working Telegram → Claude Code → Telegram loop with session lifecycle.

- Go project scaffold
- Telegram long polling and message routing
- Claude Code subprocess bridge with streaming
- Session manager with 10-minute inactivity timeout
- Basic chat-to-workspace mapping
- Home `CLAUDE.md` with identity and instructions
- Launchd service setup

**Success criteria:** Can send a message on Telegram, have a real back-and-forth conversation with Claude Code, session expires cleanly after inactivity.

### Phase 2 — Memory

Add persistence across sessions.

- SQLite memory store with typed schema
- JSONL conversation history (write on every exchange, read at session start)
- Memory extraction prompt at session close
- Cortex briefing generator (30-minute goroutine)
- Context injection block builder (briefing + history + workspace info)

**Success criteria:** Close a session, start a new one, have Claude demonstrate awareness of things discussed previously without being told.

### Phase 3 — Multi-workspace

Add proper multi-chat routing and workspace isolation.

- Config-driven chat ID → workspace mapping
- Per-workspace `CLAUDE.md` injection alongside global identity
- Workspace-scoped memory in SQLite
- `/workspace` command for manual switching

**Success criteria:** Two different Telegram chats have independent conversations and memory, with global facts (like Nate’s name) visible in both.

### Phase 4 — Structured Data

Add personal data logging and querying.

- Base schemas (workouts, meals) in SQLite
- Claude Code can query via `sqlite3` shell calls
- Schema creation on demand
- Workspace `CLAUDE.md` documents available schemas

**Success criteria:** “Log today’s workout: 45 min run” stores a record. “How many runs have I done this month?” correctly queries and responds.

### Phase 5 — Obsidian

Wire up documentation writing.

- Obsidian vault path in config and injected into `CLAUDE.md`
- Claude Code writes directly to vault via file tools
- Folder conventions documented in `CLAUDE.md`

**Success criteria:** “Document our decision to use SQLite for agent.db” creates a note in the Obsidian vault.

-----

## 13. Open Questions

1. **Memory extraction reliability.** The session-close extraction prompt could produce noisy or malformed output. Need a parsing strategy that handles partial JSON gracefully and discards bad lines without crashing.
1. **Briefing quality.** The 30-minute cortex briefing is only as good as the synthesis prompt. Will need iteration on the prompt that generates it to avoid verbosity or missing important context.
1. **Executor version stability.** Each CLI tool’s streaming format and flags can change with updates. The `Executor` interface isolates this — only the concrete implementation in `internal/executor/claude/` needs updating when Claude Code changes its protocol. Consider version-pinning strategies per executor and adding an integration test that exercises the real CLI to catch breakage early.
1. **Session resume after crash.** If the Go binary crashes mid-session, the Claude subprocess is orphaned. Need a startup check that looks for orphaned processes and a way to notify the user their last message may not have been processed.
1. **Schema evolution.** As new personal data schemas are added over time, need a lightweight migration strategy for SQLite so adding columns doesn’t break existing data.
1. **Cost visibility.** Claude Code on a Max subscription has no per-token cost, but it’s useful to log session activity for debugging. Consider logging model, approximate token usage, and session duration to SQLite.

-----

## 14. Reference Implementations Studied

- **Kai** (dcellison/kai) — closest architectural reference. Three-layer memory model, session lifecycle, context injection pattern. Python/python-telegram-bot. Wiki has excellent architecture documentation.
- **Spacebot** (spacedriveapp/spacebot) — typed memory system (8 categories), cortex briefing concept, graph edges between memories, Rust binary. Too complex for direct reference but the memory type taxonomy is worth adopting.
- **claude-telegram-relay** (godagoo) — JSONL history pattern, Supabase for semantic search. Simplified approach, good for understanding the minimal viable memory pattern.