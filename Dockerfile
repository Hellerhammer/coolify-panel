# syntax=docker/dockerfile:1.7
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/panel .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/panel /panel
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/panel"]
