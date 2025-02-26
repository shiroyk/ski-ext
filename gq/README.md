## gq
gq module provides jQuery-like selector and traversing methods.
```js
import { default as $ } from "ski/gq";

export default function () {
  return $('<div><span>hello</span></div>').find('span').text();
}
```
## References
- [goquery](https://github.com/PuerkitoBio/goquery)