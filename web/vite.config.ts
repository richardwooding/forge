import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The GUI is served from /ui/ by the Go binary, and its output is committed so
// that `go install` works with no node anywhere. Hence base, outDir, and the
// deterministic-ish file names: a bundler that renames chunks on every build
// would make the committed tree churn for no reason.
export default defineConfig({
  base: "/ui/",
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // No sourcemaps: they would double the size of something shipped inside
    // every forge binary, for a page this small.
    sourcemap: false,
  },
});
