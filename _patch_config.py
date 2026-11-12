#!/usr/bin/env python3
"""Patch config.go to add AllowRemoteAccess field."""
import re

path = "config/config.go"
with open(path, "r") as f:
    content = f.read()

# 1. Add field to struct (after AuthEnabled)
content = content.replace(
    'AuthEnabled      bool                        `json:"authEnabled"`         // 是否要求客户端携带 apiKey；默认 true，关闭后网关不鉴权\n\t\tAutoStart        bool                        `json:"autoStart"`',
    'AuthEnabled      bool                        `json:"authEnabled"`         // 是否要求客户端携带 apiKey；默认 true，关闭后网关不鉴权\n\t\tAllowRemoteAccess bool                        `json:"allowRemoteAccess"`   // 是否允许局域网远程访问（绑定 0.0.0.0 而非 127.0.0.1）\n\t\tAutoStart        bool                        `json:"autoStart"`'
)

# 2. Add to Defaults()
content = content.replace(
    'AuthEnabled:  true,\n\t\tAutoStart:    false,',
    'AuthEnabled:  true,\n\t\tAllowRemoteAccess: false,\n\t\tAutoStart:    false,'
)

# 3. Add to Clone()
content = content.replace(
    'AuthEnabled:      c.AuthEnabled,\n\t\t\tAutoStart:        c.AutoStart,',
    'AuthEnabled:      c.AuthEnabled,\n\t\t\tAllowRemoteAccess: c.AllowRemoteAccess,\n\t\t\tAutoStart:        c.AutoStart,'
)

# 4. Add to Update()
content = content.replace(
    'c.AuthEnabled = newCfg.AuthEnabled\n\t\tc.AutoStart = newCfg.AutoStart',
    'c.AuthEnabled = newCfg.AuthEnabled\n\t\tc.AllowRemoteAccess = newCfg.AllowRemoteAccess\n\t\tc.AutoStart = newCfg.AutoStart'
)

with open(path, "w") as f:
    f.write(content)

print("Done")
