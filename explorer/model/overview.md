devc runs AI coding agents in rootless Podman containers, one box per project. Everything in a box is under the agent's control; everything outside it, on the host, decides what a box may do and keeps what a box must not change. The daemon connects the host to every running box, and a hub, if there is one, shows every session of every machine in a browser.

This page is a placeholder: later chunks write the overview and the reading order. Until then, start with the runtime map, then [[concern:sessions-and-events]], the flow [[flow:send-prompt]] and the tour [[flow:box-channel]].
