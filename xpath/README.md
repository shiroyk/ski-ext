## xpath
xpath module provides selecting nodes from XML, HTML or other documents using XPath expression.
```js
import xpath from "ski/xpath";

export default () => {
  console.log(xpath('//span').innerText("<div><span>hello</span></div>"));
}
```
## References
- [htmlquery](https://github.com/antchfx/htmlquery)
- [xpath](https://github.com/antchfx/xpath)