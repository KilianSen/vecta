# Real headless Minecraft clients (HeadlessMC) for end-to-end tests.
FROM 3arthqu4ke/headlessmc:2.10.0
# Xvfb + Mesa for mods that need real GL (Create/Flywheel); xrandr is required
# by LWJGL2 (1.7.10-1.12.2) under Xvfb.
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      xvfb xauth x11-xserver-utils libgl1-mesa-dri libglx-mesa0 libgl1 curl jq \
    && rm -rf /var/lib/apt/lists/*
COPY hmc-run.sh /usr/local/bin/hmc-run.sh
RUN chmod 0755 /usr/local/bin/hmc-run.sh
ENTRYPOINT ["/usr/local/bin/hmc-run.sh"]
