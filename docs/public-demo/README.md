# AMAC public lab

Synthetic browser model of AMAC delivery monitoring. It never connects to a Mac, agent, terminal, job tracker, email account, or notification service.

Serve this directory with a static HTTP server. Publish only this directory as the Vercel project root, using its vercel.json. It contains no application API or service credentials.

This educational model is separate from the native event journal and scheduler. It demonstrates the delivery-health distinction, not a live machine or operator study.

Input limits are enforced by the local demo engine. Reset or reload discards state. CSP limits scripts, workers, and connections to this origin. No dependencies are loaded from a CDN.
