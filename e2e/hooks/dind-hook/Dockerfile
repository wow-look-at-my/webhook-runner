FROM docker:28-dind
COPY dind-smoke.sh /usr/local/bin/dind-smoke.sh
RUN chmod +x /usr/local/bin/dind-smoke.sh
# Reset the base image's dockerd-entrypoint.sh. This hook never starts a
# daemon: the container command IS the contract check, and the declared test
# invokes the same script.
ENTRYPOINT []
CMD ["/usr/local/bin/dind-smoke.sh"]
