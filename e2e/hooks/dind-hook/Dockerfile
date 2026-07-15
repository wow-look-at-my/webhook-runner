FROM docker:28-dind
COPY dind-smoke.sh /usr/local/bin/dind-smoke.sh
RUN chmod +x /usr/local/bin/dind-smoke.sh
# Reset the base image's dockerd-entrypoint.sh: this hook's own script
# starts the nested daemon itself, so the container command IS the smoke
# check (the live-run CMD), and the declared test invokes the same script.
ENTRYPOINT []
CMD ["/usr/local/bin/dind-smoke.sh"]
