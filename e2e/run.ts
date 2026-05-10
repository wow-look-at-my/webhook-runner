await exec.exec("npm", ["install"], { cwd: "e2e" });
await exec.exec("node", ["--import", "tsx", "--test", "e2e.test.ts"], {
  cwd: "e2e",
});
