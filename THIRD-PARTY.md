Third-party components
======================

Dependencies whose license needs a recorded decision. Everything not listed here
is MIT, Apache-2.0, BSD or ISC and needs none.

github.com/eclipse/paho.mqtt.golang
-----------------------------------

- Version: v1.4.2, pulled in transitively through
  `github.com/SENERGY-Platform/mgw-dc-lib-go`, which every mgw device connector
  uses for its MQTT client.
- License: dual, Eclipse Public License v2.0 **or** Eclipse Distribution License
  v1.0. Both texts ship with the module as `epl-v20` and `edl-v10`, and every
  source file names both.
- **We take it under EDL-1.0**, recorded 2026-11-26. EDL-1.0 is BSD-3-Clause
  verbatim: keep the copyright notice on source and binary redistribution, and
  do not use the Eclipse Foundation's name to endorse derived products. No
  copyleft obligation follows, and nothing in it conflicts with the Apache-2.0
  this connector is released under.
- Automated scanners report this dependency as EPL-2.0, because its `LICENSE`
  file opens with the EPL text. Upstream says why in that file's header: the EPL
  copy is there to pass the pkg.go.dev license check. The EDL option is the one
  that applies here.
