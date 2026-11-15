<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Plain-words help for the "expected fingerprint" fields of "add server" and "join cluster".
defineProps<{ purpose: 'add' | 'join' }>()
</script>

<template>
  <details class="fingerprint-help">
    <summary>Where do I find the fingerprint, and what does it protect against?</summary>
    <div class="stack" style="gap: var(--s-2)">
      <p v-if="purpose === 'add'">
        <strong>Where to read it.</strong> It is the SHA-256 fingerprint of the certificate of the
        <em>new</em> server. That server writes it to its log when it starts, in the line "bootstrap
        certificate of the internal API"; it is also the SHA-256 of the file
        <code>pki/bootstrap.crt</code> in its data directory. Copy it from the new server itself,
        not from this screen.
      </p>
      <p v-else>
        <strong>Where to read it.</strong> It is the SHA-256 fingerprint of the internal certificate
        of the <em>existing</em> server you join: the SHA-256 of the file
        <code>pki/server.crt</code> in that server's data directory. Read it on that server, not
        from this screen.
      </p>
      <p>
        <strong>What it protects against.</strong> The first contact between two servers has no
        common authority to vouch for either side. Without a fingerprint, someone on the network
        path, or an address that points to the wrong machine, can answer in the other server's place
        and receive
        {{
          purpose === 'add'
            ? "the new server's setup token"
            : "the administrator's password you type here"
        }}. With a fingerprint, the connection is refused before anything secret is sent if the
        certificate is not the one you named.
      </p>
      <p>
        Leave it empty only on a network you control. Without it the cluster records the fingerprint
        it saw, so you can compare it afterwards; that detects a problem but does not prevent it.
        <strong
          >Give the fingerprint whenever the address was not typed from your own records.</strong
        >
      </p>
    </div>
  </details>
</template>
