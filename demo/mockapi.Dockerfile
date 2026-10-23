# syntax=docker/dockerfile:1

# Mock Servers.com API for the demo. Build context is src/; the ignore file
# next to this Dockerfile (mockapi.Dockerfile.dockerignore) replaces
# src/.dockerignore, which leaves test/ out.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY test/mockapi/ test/mockapi/
RUN CGO_ENABLED=0 go build -trimpath -o /out/mockapi ./test/mockapi

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/mockapi /usr/bin/mockapi
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/bin/mockapi"]
