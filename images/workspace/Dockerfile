# anvil/workspace:ubuntu-24.04 — the base image every sandbox container
# runs from.
#
# Deliberately left out: Docker CLI, sudo, ssh client, anything that could
# escalate privileges or reach outside the container. The container never
# gets the permissions that would make these tools dangerous anyway, but
# there's no reason to ship them either.
#
# Rebuilt regularly by CI (.github/workflows/image-rebuild.yml) and
# referenced everywhere else by digest, never by this floating tag.
FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        ripgrep \
        make \
        build-essential \
        python3 \
        python3-pip \
    && rm -rf /var/lib/apt/lists/*

# Go 1.23
RUN curl -fsSL https://go.dev/dl/go1.23.4.linux-$(dpkg --print-architecture).tar.gz -o /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz
ENV PATH="/usr/local/go/bin:${PATH}"

# Node 22
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# The container always runs as this non-root user, never as root.
RUN useradd --uid 10001 --create-home --shell /bin/bash sandbox

WORKDIR /workspace
USER 10001

CMD ["/bin/bash"]
