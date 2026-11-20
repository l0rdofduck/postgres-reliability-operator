FROM golang:1.24 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/operator ./cmd

FROM scratch

COPY --from=build /out/operator /operator

USER 65532:65532

ENTRYPOINT ["/operator"]
