# curl for the POST, jq to read one field out of the settings document --
# parsing JSON with sed in a manifest string is exactly what the shell-in-JSON
# ban exists to stop.
FROM alpine:3.20
RUN apk add --no-cache curl jq
WORKDIR /app
COPY notify.sh ./
