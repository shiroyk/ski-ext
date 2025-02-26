## jq
jq module provides JSON path expressions for filtering and extracting JSON elements.
```js
import jq from "ski/jq";

export default () => {
  let data = JSON.parse(`{"hello": 1}`);
  console.log(jq('$.hello').get(data));
}
```
## References
- [ojg](https://github.com/ohler55/ojg)