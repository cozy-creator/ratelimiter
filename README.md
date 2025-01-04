A minimalist distributed key-value store used to rate-limit APIs using a token bucket algorithm.

We use Hashicorp's [memberlist](https://pkg.go.dev/github.com/hashicorp/memberlist) library to implement a gossip protocol.

## Usage

```
go run main.go
```

