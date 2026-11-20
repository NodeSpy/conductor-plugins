FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server && rm -rf /var/lib/apt/lists/* \
 && useradd -m -s /bin/bash bk && mkdir -p /run/sshd /home/bk/.ssh && chown -R bk /home/bk/.ssh && chmod 700 /home/bk/.ssh \
 && ssh-keygen -A \
 && printf 'PasswordAuthentication no\nPermitRootLogin no\n' > /etc/ssh/sshd_config.d/e2e.conf
ARG BORG_URL=https://github.com/borgbackup/borg/releases/download/1.4.1/borg-linux-glibc236
ADD --chmod=755 ${BORG_URL} /usr/local/bin/borg
CMD ["/bin/sh","-c","cat /keys/authorized_keys > /home/bk/.ssh/authorized_keys && chown bk /home/bk/.ssh/authorized_keys && chmod 600 /home/bk/.ssh/authorized_keys && exec /usr/sbin/sshd -D -e"]
