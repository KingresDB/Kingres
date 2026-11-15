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
        <em>new</em> server. Open the panel of the new server itself: its setup wizard shows the
        fingerprint (with a copy button) as soon as you have entered its setup token there (or when
        you open it on that machine itself). Copy it from there, not from this screen. If that panel
        does not show it (it says so), read it in the log of the new server, in the line "bootstrap
        certificate of the internal API", or take the SHA-256 of the file
        <code>pki/bootstrap.crt</code> in its data directory.
      </p>
      <p v-else>
        <strong>Where to read it.</strong> It is the SHA-256 fingerprint of the internal certificate
        of the <em>existing</em> server you join. Open the panel of that server and read it on its
        <em>This server</em> page (with a copy button). If that page does not show it, read the
        SHA-256 of the file <code>pki/server.crt</code> in that server's data directory. Do not read
        it from this screen.
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
        The fingerprint is required. The only way around it is the explicit choice below it, which
        trusts whatever answers at the address.
        <strong
          >Give the fingerprint whenever the address was not typed from your own records.</strong
        >
      </p>
    </div>
  </details>
</template>
