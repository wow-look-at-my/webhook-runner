// Relative sdk import — the tree-mirror COPY convention keeps this path
// valid both in-repo and inside the built image.
import { greet } from '../../sdk/util.ts';

console.log(greet('webhook-runner'));
