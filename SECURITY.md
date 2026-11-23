# Security Policy

## Supported versions

Only the latest commit on `main` gets security fixes.

## Reporting a vulnerability

Do not open a public issue for security problems.

Report privately through
[GitHub private vulnerability reporting](https://github.com/fukubie/StockLlama/security/advisories/new).
Include the affected version or commit, steps to reproduce, and the impact.

We aim to confirm reports within 7 days and to fix confirmed problems within 30 days.

## Scope

StockLlama is a local app. These areas matter most:

- The local HTTP server: host and origin checks, the session token, and CSP
- Handling of API keys, which must never be written to disk, logs, or exports
- Stockfish download and extraction
- Parsing of imported PGN, CSV, and JSON files
- Starting external processes such as Stockfish, Ollama, and PowerShell
