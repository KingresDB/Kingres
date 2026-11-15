<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Shown on the sign-in page while the system database of this server is stopped on purpose.
//
// The contract (startThisServerInstance, stopThisServerInstance) gives a person who is not signed in no
// way to start the instance: login and every operation that needs a session answer 503
// `system_database_stopped`, and `startThisServerInstance` needs a valid session and its CSRF token,
// which a reloaded page no longer has (`getSession`, which returns the token, answers 503 as well). So
// this page does not offer a button; it says exactly what can be done instead.
</script>

<template>
  <section class="notice tone-warn" role="alert" data-testid="system-database-stopped-signin">
    <span aria-hidden="true">&#9650;</span>
    <div class="stack" style="gap: var(--s-2)">
      <strong>The system database is stopped</strong>
      <span>
        On this server nobody can sign in until it runs again. Your user name and password are not
        the problem. This page cannot start it for you: starting an instance needs a signed-in
        session, and a new session needs the system database.
      </span>
      <span>What you can do:</span>
      <ul class="steps">
        <li>
          If a tab of this panel that was signed in before the instance was stopped is still open,
          use it: it shows a <strong>Start instance</strong> button.
        </li>
        <li>
          Otherwise start the instance on the machine of this server, for example with
          <code>pg_ctl start</code> for its data directory, or restart the
          <code>kingresd</code> service of this server, which starts the instance again. The server
          notices a start made on the machine within a few seconds.
        </li>
        <li>
          The panel of another server starts only the instances of that server, so it does not help
          for this one.
        </li>
      </ul>
      <span>When the instance runs again, sign in as usual.</span>
    </div>
  </section>
</template>

<style scoped>
.steps {
  margin: 0;
  padding-left: 1.2em;
}
</style>
