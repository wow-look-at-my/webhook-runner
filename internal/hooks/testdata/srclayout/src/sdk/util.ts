// Shared dependency-free code: imported relatively by hooks, never
// published, no package.json. Editing this re-tags EVERY src-layout hook.
export function greet(name: string): string {
	return `hello from the sdk, ${name}`;
}
