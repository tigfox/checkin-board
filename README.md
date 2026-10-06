# checkin-board

Go app that talks to a locally running HTTP/JSON API.

## Layout

```
cmd/checkin-board/     entrypoint
internal/config/       env-based configuration
internal/apiclient/    HTTP client (Get/Post helpers, typed errors)
```

## Configuration

| Variable       | Default                 | Description              |
|----------------|-------------------------|--------------------------|
| `API_BASE_URL` | `http://localhost:8080` | Base URL of the local API |
| `API_TIMEOUT`  | `10s`                   | Per-request timeout       |

## Usage

```sh
make test     # run tests with -race
make cover    # coverage report
make run      # hits GET /health on the API
```

Add endpoint methods to `internal/apiclient` alongside `Health`.
