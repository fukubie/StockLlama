# StockLlama

A local chess trainer built with Go and Stockfish

## Run

On Windows open `start.cmd`
For a fresh clone install [Go](https://go.dev/dl/) 1.26 or newer first
Use Settings to install Stockfish or choose an existing executable

On other systems build and run from the project directory

```sh
go build -trimpath -ldflags="-s -w" -o bin/stockllama .
./bin/stockllama
```

The app prints its local address
It accepts connections only from your own device
Set `STOCKLLAMA_ADDR` to use another local address such as `localhost:8765` or `[::1]:8765`

## Prepare for a player

Enter a username and fetch recent games
The app recommends up to three games based on the sample
You can reduce that number before analysis
The percentage shows completed analysis work

Recent imports are cached for one day and can be reused offline
Saved profiles remain available until you remove them
Exports contain chess data without local paths or API keys

PGN files need matching White or Black player names
CSV files need a pgn column with quoted full game text
JSON accepts a game array or an exported StockLlama profile
The import screen samples up to eight games and recommends a smaller analysis

Use the synthetic [demo game](examples/demo.pgn) with the name PracticePartner

## Coaching

Stockfish provides offline move recommendations without a language model
The right panel gives a short reply based on the current position
Saved mistakes are cited when the position matches
A single mistake is described as one example rather than a proven weakness

Ollama and compatible API services are optional
Detect models in Settings or enter a model name
Ask uses the selected model while automatic hints stay with Stockfish
An API request sends the position summary to your chosen provider
Keys remain in memory and disappear when the app stops

The Ollama folder is detected on each device
It appears only in the local settings screen
Paths are absent from shared defaults and profile exports

Use GPU allows Ollama to choose GPU offloading
Turning it off requests CPU inference
A checkmark confirms detected hardware
GPU active appears only when Ollama reports video memory for the selected model
Stockfish uses the CPU
API inference runs on the provider hardware

## Resource limits

Stockfish uses one thread and a 16 MB hash table
Quick analysis studies the first 24 player moves in each selected game
All observed moves still enter the player model
Repeated positions share cached analysis
The play engine is reused until its path changes or the app stops
Progress polling sends a small status object

Local model replies have a short output budget and a 2048 token context
Models are kept loaded for 30 seconds after a request
Move modeling uses observed frequencies rather than neural network training
Short searches are approximate and other replies may also be good

## Tests

```sh
go test -cover ./...
go vet ./...
```

Set `STOCKLLAMA_TEST_ENGINE` to your engine executable for real engine checks
Browser checks need a running app and a Chromium browser

```sh
npm ci
npm run test:gui
```

Set `CHROME_PATH` if Chrome is installed somewhere else
Set `STOCKLLAMA_LIVE_TEST=1` to include a live username import

## Contribute

Read [the contributing guide](CONTRIBUTING.md)
Source uses [GPLv3 or later](LICENSE)
The chess library is [notnil chess](https://github.com/notnil/chess) under MIT
Downloaded Stockfish binaries retain their GPL license
Engines and model weights are stored locally and ignored by Git

## References

[Chess public API](https://support.chess.com/en/articles/9650547-what-is-the-pubapi-and-how-do-i-use-it)

[Ollama model listing](https://docs.ollama.com/api/tags)

[Ollama GPU status](https://docs.ollama.com/api/ps)
