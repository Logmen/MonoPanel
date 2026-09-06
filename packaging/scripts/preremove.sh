#!/bin/sh
systemctl disable --now monopanel-api.service monopanel-agent.service >/dev/null 2>&1 || true
