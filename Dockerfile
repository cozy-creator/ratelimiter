FROM golang:1.21-alpine

WORKDIR /app

# Copy go mod files
COPY go.mod ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN go build -o ratelimiter

# Run the application
CMD ["./ratelimiter"] 