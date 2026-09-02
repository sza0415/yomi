/// <reference types="vite/client" />

declare module "*.vue" {
  import type { DefineComponent } from "vue";
  const component: DefineComponent<Record<string, never>, Record<string, never>, unknown>;
  export default component;
}

declare module "@lucide/vue/dist/esm/icons/*.mjs" {
  import type { FunctionalComponent, SVGAttributes } from "vue";
  const icon: FunctionalComponent<SVGAttributes & { size?: number | string; strokeWidth?: number | string }>;
  export default icon;
}
